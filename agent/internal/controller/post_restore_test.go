// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/lifecyclehooks"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const postRestoreMarker = "/app-release/released"

// postRestoreHarness drives runRestore with a post-restore action. The
// destination container's files are a set, and the restore executor, the
// marker, HTTP and the wait between attempts are all fakes.
type postRestoreHarness struct {
	t    *testing.T
	w    *NodeController
	pod  *corev1.Pod
	plan *restorePlan

	mu           sync.Mutex
	files        map[string]bool
	events       []string // sentinels written, markers created, and kills, in order
	restores     int
	httpResults  []error // one per POST; the last one repeats
	httpIP       string
	httpTimeout  time.Duration
	waits        int
	maxWaits     int // a wait after this many fails as if the deadline passed
	restoreAdds  string
	completedAt  time.Time
	completedErr error
}

func newPostRestoreHarness(t *testing.T, action *snapshotv1alpha1.PostRestoreAction) *postRestoreHarness {
	t.Helper()
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.PodIP = "10.0.0.9"
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "app-release", MountPath: "/app-release"}}
	pod.Spec.Volumes = []corev1.Volume{{Name: "app-release", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	h := &postRestoreHarness{
		t: t, pod: pod, files: map[string]bool{}, maxWaits: 3,
		plan: &restorePlan{
			artifact:    &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"},
			postRestore: action,
		},
	}
	w := makeTestController(t, pod)
	w.config.Restore.RestoreTimeoutSeconds = 60
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.restores++
		if h.restoreAdds != "" {
			h.files[h.restoreAdds] = true // the rootfs diff brought it back
		}
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 77}, nil
	}
	w.writeControlSentinelFn = func(_ int, name string, _ []byte) error { h.record("sentinel " + name); return nil }
	w.sendSignalFn = func(logr.Logger, int, syscall.Signal, string) error { h.record("kill"); return nil }
	w.probeFileFn = func(_ int, path string) (bool, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.files[path], nil
	}
	w.createMarkerFn = func(_ int, path string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.files[path] {
			return lifecyclehooks.ErrExists
		}
		h.files[path] = true
		h.events = append(h.events, "marker "+path)
		return nil
	}
	w.postRestoreHTTPFn = func(_ context.Context, ip string, _ *snapshotv1alpha1.HTTPPostAction, timeout time.Duration) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.httpIP, h.httpTimeout = ip, timeout
		h.events = append(h.events, "post")
		if len(h.httpResults) == 0 {
			return nil
		}
		err := h.httpResults[0]
		if len(h.httpResults) > 1 {
			h.httpResults = h.httpResults[1:]
		}
		return err
	}
	w.postRestoreWaitFn = func(ctx context.Context, _ time.Duration) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.waits++
		if h.waits > h.maxWaits {
			return context.DeadlineExceeded
		}
		return ctx.Err()
	}
	w.controlSentinelModTimeFn = func(_ int, name string) (time.Time, error) {
		assert.Equal(t, podcontract.RestoreCompleteFile, name)
		return h.completedAt, h.completedErr
	}
	h.w = w
	return h
}

func (h *postRestoreHarness) record(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event)
}

func (h *postRestoreHarness) run(recovering bool) error {
	return h.w.runRestore(context.Background(), h.pod, h.plan, "main", "ctr-abc", time.Now(), recovering)
}

func (h *postRestoreHarness) recordedEvents() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func filePostRestore(path string) *snapshotv1alpha1.PostRestoreAction {
	return &snapshotv1alpha1.PostRestoreAction{
		PostRestoreHandler: snapshotv1alpha1.PostRestoreHandler{File: &snapshotv1alpha1.ControlFileAction{Path: path}},
		PeriodSeconds:      1, TimeoutSeconds: 1,
	}
}

func httpPostRestore() *snapshotv1alpha1.PostRestoreAction {
	return &snapshotv1alpha1.PostRestoreAction{
		PostRestoreHandler: snapshotv1alpha1.PostRestoreHandler{
			HTTPPost: &snapshotv1alpha1.HTTPPostAction{Path: "/checkpoint/resume", Port: 8000},
		},
		PeriodSeconds: 1, TimeoutSeconds: 4,
	}
}

func recoveringPod(h *postRestoreHarness) {
	h.pod.Status.Conditions = append(h.pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodConditionType(podcontract.RestoredCondition),
		Status: corev1.ConditionFalse,
		Reason: podcontract.RestoreReasonInProgress,
	})
	h.w.controlSentinelExistsFn = func(int, string) (bool, error) { return true, nil }
}

func TestPostRestoreAction(t *testing.T) {
	content := &snapshotv1alpha1.PodSnapshotContent{Spec: snapshotv1alpha1.PodSnapshotContentSpec{
		Containers: []snapshotv1alpha1.ContainerSnapshotConfig{
			{Name: "other", Lifecycle: &snapshotv1alpha1.SnapshotLifecycle{PostRestore: httpPostRestore()}},
			{Name: "main", Lifecycle: &snapshotv1alpha1.SnapshotLifecycle{PostRestore: filePostRestore(postRestoreMarker)}},
		},
	}}
	got := postRestoreAction(content, "main")
	require.NotNil(t, got)
	assert.Equal(t, postRestoreMarker, got.File.Path, "the source container's own action")
	assert.Nil(t, postRestoreAction(content, "missing"))
	assert.Nil(t, postRestoreAction(&snapshotv1alpha1.PodSnapshotContent{}, "main"))
}

func TestPostRestoreFileIsCreatedAfterRestoreComplete(t *testing.T) {
	h := newPostRestoreHarness(t, filePostRestore(postRestoreMarker))

	require.NoError(t, h.run(false))

	assert.Equal(t, []string{"sentinel " + podcontract.RestoreCompleteFile, "marker " + postRestoreMarker}, h.recordedEvents())
}

func TestPostRestoreFileThatExistsBeforeRestoreFailsWithoutCRIU(t *testing.T) {
	h := newPostRestoreHarness(t, filePostRestore(postRestoreMarker))
	h.files[postRestoreMarker] = true

	err := h.run(false)

	require.Error(t, err)
	assert.Equal(t, executor.StepCheckPostRestore, executor.StepOf(err))
	assert.Contains(t, err.Error(), "already exists")
	assert.Equal(t, 0, h.restores, "no CRIU work for a restore that can't notify its application")
	assert.Equal(t, []string{"sentinel " + podcontract.RestoreFailedFile, "kill"}, h.recordedEvents())
}

func TestPostRestoreFileThatTheRestoreBringsBackFails(t *testing.T) {
	h := newPostRestoreHarness(t, filePostRestore(postRestoreMarker))
	h.restoreAdds = postRestoreMarker

	err := h.run(false)

	require.Error(t, err)
	assert.Equal(t, executor.StepCheckPostRestore, executor.StepOf(err))
	assert.Equal(t, 1, h.restores)
	assert.NotContains(t, h.recordedEvents(), "sentinel "+podcontract.RestoreCompleteFile,
		"the restored process must not be released when the marker is already there")
	assert.Equal(t, []string{"sentinel " + podcontract.RestoreFailedFile, "kill"}, h.recordedEvents())
}

func TestPostRestoreRefusedTargetsFailWithoutCRIU(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action *snapshotv1alpha1.PostRestoreAction
		mutate func(*corev1.Pod)
	}{
		{"file on a PVC", filePostRestore("/data/released"), func(pod *corev1.Pod) {
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/data"})
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"},
			}})
		}},
		{"Snapshot's own control file", filePostRestore("/snapshot-control/" + podcontract.RestoreCompleteFile), func(pod *corev1.Pod) {
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name: podcontract.SnapshotControlVolumeName, MountPath: podcontract.SnapshotControlMountPath,
			})
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: podcontract.SnapshotControlVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}},
		{"HTTP on the host network", httpPostRestore(), func(pod *corev1.Pod) { pod.Spec.HostNetwork = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPostRestoreHarness(t, tc.action)
			tc.mutate(h.pod)

			err := h.run(false)

			require.Error(t, err)
			assert.Equal(t, executor.StepCheckPostRestore, executor.StepOf(err))
			assert.Equal(t, 0, h.restores)
		})
	}
}

func TestPostRestoreHTTPRetriesUntilAccepted(t *testing.T) {
	h := newPostRestoreHarness(t, httpPostRestore())
	h.httpResults = []error{
		&lifecyclehooks.StatusError{Method: http.MethodPost, Code: http.StatusNotFound},
		errors.New("connection refused"),
		nil,
	}

	require.NoError(t, h.run(false))

	assert.Equal(t, []string{"sentinel " + podcontract.RestoreCompleteFile, "post", "post", "post"}, h.recordedEvents())
	assert.Equal(t, "10.0.0.9", h.httpIP)
	assert.Equal(t, 4*time.Second, h.httpTimeout)
}

func TestPostRestoreThatNeverSucceedsFailsTheRestore(t *testing.T) {
	h := newPostRestoreHarness(t, httpPostRestore())
	h.httpResults = []error{errors.New("connection refused")}

	err := h.run(false)

	require.Error(t, err)
	assert.Equal(t, executor.StepPostRestore, executor.StepOf(err))
	assert.Contains(t, err.Error(), "connection refused")
	events := h.recordedEvents()
	assert.Equal(t, "sentinel "+podcontract.RestoreCompleteFile, events[0])
	assert.Equal(t, []string{"sentinel " + podcontract.RestoreFailedFile, "kill"}, events[len(events)-2:],
		"a restored process whose application was never notified is stopped")
}

func TestPostRestoreRecoveryRunsOnlyThePostRestoreStep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action *snapshotv1alpha1.PostRestoreAction
		marker bool
		want   []string
	}{
		{"file not created yet", filePostRestore(postRestoreMarker), false, []string{"marker " + postRestoreMarker}},
		{"file created before the restart", filePostRestore(postRestoreMarker), true, nil},
		{"HTTP", httpPostRestore(), false, []string{"post"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPostRestoreHarness(t, tc.action)
			recoveringPod(h)
			h.completedAt = time.Now()
			h.files[postRestoreMarker] = tc.marker

			require.NoError(t, h.run(true))

			assert.Equal(t, 0, h.restores, "CRIU must never run again")
			assert.Equal(t, tc.want, h.recordedEvents())
		})
	}
}

func TestPostRestoreRecoveryKeepsTheOriginalDeadline(t *testing.T) {
	h := newPostRestoreHarness(t, httpPostRestore())
	recoveringPod(h)
	h.completedAt = time.Now().Add(-2 * time.Minute) // the 60s restore deadline has passed
	h.httpResults = []error{errors.New("connection refused")}
	h.maxWaits = 100
	h.w.postRestoreWaitFn = nil // the real wait sees the expired deadline

	err := h.run(true)

	require.Error(t, err)
	assert.Equal(t, executor.StepPostRestore, executor.StepOf(err))
	assert.Equal(t, []string{"post", "sentinel " + podcontract.RestoreFailedFile, "kill"}, h.recordedEvents(),
		"one attempt, then the expired deadline fails the restore")
}

func TestPostRestoreRecoveryWithoutACompletionTimeFails(t *testing.T) {
	h := newPostRestoreHarness(t, filePostRestore(postRestoreMarker))
	recoveringPod(h)
	h.completedErr = errors.New("stat failed")

	err := h.run(true)

	require.Error(t, err)
	assert.Equal(t, executor.StepPostRestore, executor.StepOf(err))
	assert.Equal(t, 0, h.restores)
}

func TestNoPostRestoreLeavesRecoveryAsBefore(t *testing.T) {
	h := newPostRestoreHarness(t, nil)
	recoveringPod(h)
	h.w.controlSentinelModTimeFn = func(int, string) (time.Time, error) {
		t.Fatal("no post-restore action, so nothing reads the completion time")
		return time.Time{}, nil
	}

	require.NoError(t, h.run(true))
	assert.Empty(t, h.recordedEvents())
}
