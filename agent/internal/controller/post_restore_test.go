// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const postRestoreMarker = "/app-release/released"

// postRestoreHarness drives runRestore with a post-restore action. The
// destination container's filesystem is a temp dir, where the file actions'
// scripts run with the local /bin/sh. The restore executor, HTTP and the wait
// between attempts are fakes.
type postRestoreHarness struct {
	t    *testing.T
	w    *NodeController
	pod  *corev1.Pod
	plan *restorePlan
	root string // the destination container's filesystem

	mu          sync.Mutex
	events      []string // sentinels written, markers created, POSTs and kills, in order
	restores    int
	httpResults []int // a status per POST, 0 for a connection error; the last one repeats
	httpURL     string
	httpTimeout time.Duration
	waits       int
	waitPeriods []time.Duration // the period passed to each wait
	maxWaits    int             // a wait after this many fails as if the deadline passed
	restoreAdds string
	restoreDir  string // a directory the restored files bring
}

func newPostRestoreHarness(t *testing.T, action *snapshotv1alpha1.PostRestoreAction) *postRestoreHarness {
	t.Helper()
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	pod.Status.PodIP = "10.0.0.9"
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "app-release", MountPath: "/app-release"}}
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "hooks", ContainerPort: 8000}}
	pod.Spec.Volumes = []corev1.Volume{{Name: "app-release", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	h := &postRestoreHarness{t: t, pod: pod, root: t.TempDir(), maxWaits: 3}
	require.NoError(t, os.MkdirAll(filepath.Join(h.root, "app-release"), 0o755))
	w := makeTestController(t, pod)
	w.config.Restore.TimeoutSeconds = 60
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
		h.mu.Lock()
		h.restores++
		h.mu.Unlock()
		if h.restoreDir != "" {
			require.NoError(t, os.MkdirAll(filepath.Join(h.root, h.restoreDir), 0o755))
		}
		if h.restoreAdds != "" {
			h.addFile(h.restoreAdds) // the rootfs diff brought it back
		}
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 77}, nil
	}
	w.writeControlSentinelFn = func(_ int, name string, _ []byte) error { h.record("sentinel " + name); return nil }
	w.sendSignalFn = func(logr.Logger, int, syscall.Signal, string) error { h.record("kill"); return nil }
	w.hookExec = h
	w.hookHTTP = &http.Client{Transport: h}
	w.postRestoreWaitFn = func(ctx context.Context, period time.Duration) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.waits++
		h.waitPeriods = append(h.waitPeriods, period)
		if h.waits > h.maxWaits {
			return context.DeadlineExceeded
		}
		return ctx.Err()
	}
	h.w = w
	handler, period, err := newPostRestoreHandler(action, w.hookExec, w.hookHTTP)
	require.NoError(t, err)
	h.plan = &restorePlan{
		artifact:          &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"},
		postRestore:       handler,
		postRestorePeriod: period,
	}
	return h
}

// ExecSync runs a file action's command as the runtime would, with the path
// moved under the harness's root, and reports a non-zero exit the way
// cri-client does. The no-op that finds the shell always succeeds.
func (h *postRestoreHarness) ExecSync(ctx context.Context, containerID string, cmd []string, _ time.Duration) ([]byte, []byte, error) {
	require.Equal(h.t, "ctr-abc", containerID, "file actions run in the destination container")
	if cmd[len(cmd)-1] == "exit 0" {
		return nil, nil, nil
	}
	path := cmd[len(cmd)-1]
	args := append(append([]string{}, cmd[1:len(cmd)-1]...), filepath.Join(h.root, path))
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, cmd[0], args...)
	c.Stderr = &stderr
	err := c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil, stderr.Bytes(), exitStatus(exit.ExitCode())
	}
	if err == nil && strings.Contains(cmd[2], "set -C") {
		h.record("marker " + path)
	}
	return nil, stderr.Bytes(), err
}

type exitStatus int

func (e exitStatus) Error() string   { return fmt.Sprintf("exited with %d", int(e)) }
func (e exitStatus) ExitStatus() int { return int(e) }

// RoundTrip answers a POST with the next scripted status.
func (h *postRestoreHarness) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.httpURL = r.URL.String()
	if deadline, ok := r.Context().Deadline(); ok {
		h.httpTimeout = time.Until(deadline)
	}
	h.events = append(h.events, r.Method)
	code := http.StatusOK
	if len(h.httpResults) > 0 {
		code = h.httpResults[0]
		if len(h.httpResults) > 1 {
			h.httpResults = h.httpResults[1:]
		}
	}
	if code == 0 {
		return nil, errors.New("connection refused")
	}
	return &http.Response{StatusCode: code, Body: http.NoBody, Request: r}, nil
}

func (h *postRestoreHarness) addFile(path string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(filepath.Join(h.root, path), nil, 0o644))
}

func (h *postRestoreHarness) hasFile(path string) bool {
	_, err := os.Stat(filepath.Join(h.root, path))
	return err == nil
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

func filePostRestoreSpec(path string) *snapshotv1alpha1.PostRestoreAction {
	return &snapshotv1alpha1.PostRestoreAction{
		PostRestoreHandler: snapshotv1alpha1.PostRestoreHandler{File: &snapshotv1alpha1.ControlFileAction{Path: path}},
		PeriodSeconds:      1, TimeoutSeconds: 1,
	}
}

func httpPostRestoreSpec() *snapshotv1alpha1.PostRestoreAction {
	return httpPostRestoreSpecOn(intstr.FromInt32(8000))
}

func httpPostRestoreSpecOn(port intstr.IntOrString) *snapshotv1alpha1.PostRestoreAction {
	return &snapshotv1alpha1.PostRestoreAction{
		PostRestoreHandler: snapshotv1alpha1.PostRestoreHandler{
			HTTPPost: &snapshotv1alpha1.HTTPPostAction{Path: "/checkpoint/resume", Port: port},
		},
		PeriodSeconds: 1, TimeoutSeconds: 4,
	}
}

func httpPostRestoreSpecWithPath(path string) *snapshotv1alpha1.PostRestoreAction {
	action := httpPostRestoreSpec()
	action.HTTPPost.Path = path
	return action
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
			{Name: "other", Lifecycle: &snapshotv1alpha1.SnapshotLifecycle{PostRestore: httpPostRestoreSpec()}},
			{Name: "main", Lifecycle: &snapshotv1alpha1.SnapshotLifecycle{PostRestore: filePostRestoreSpec(postRestoreMarker)}},
		},
	}}
	got := postRestoreAction(content, "main")
	require.NotNil(t, got)
	assert.Equal(t, postRestoreMarker, got.File.Path, "the source container's own action")
	assert.Nil(t, postRestoreAction(content, "missing"))
	assert.Nil(t, postRestoreAction(&snapshotv1alpha1.PodSnapshotContent{}, "main"))
}

func TestPostRestoreFileIsCreatedAfterRestoreComplete(t *testing.T) {
	h := newPostRestoreHarness(t, filePostRestoreSpec(postRestoreMarker))

	require.NoError(t, h.run(false))

	assert.Equal(t, []string{"sentinel " + podcontract.RestoreCompleteFile, "marker " + postRestoreMarker}, h.recordedEvents())
	assert.True(t, h.hasFile(postRestoreMarker))
}

func TestPostRestoreFileThatExistsBeforeTheRestoreCountsAsTheNotification(t *testing.T) {
	// Nothing checks the file before the release, to keep the restore fast.
	// The workload contract forbids such a file.
	for _, tc := range []struct {
		name  string
		setup func(*postRestoreHarness)
	}{
		{"already in the container", func(h *postRestoreHarness) { h.addFile(postRestoreMarker) }},
		{"brought back by the restored files", func(h *postRestoreHarness) { h.restoreAdds = postRestoreMarker }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPostRestoreHarness(t, filePostRestoreSpec(postRestoreMarker))
			tc.setup(h)

			require.NoError(t, h.run(false))

			assert.Equal(t, []string{"sentinel " + podcontract.RestoreCompleteFile}, h.recordedEvents(),
				"the process is released, and Snapshot creates no file")
		})
	}
}

func TestPostRestoreFileThatCantBeCreatedFailsAfterRelease(t *testing.T) {
	h := newPostRestoreHarness(t, filePostRestoreSpec("/no-such-dir/released"))

	err := h.run(false)

	require.Error(t, err)
	assert.Equal(t, executor.StepPostRestore, executor.StepOf(err))
	assert.Contains(t, err.Error(), "failed to create", "the error says why the file can't be created")
	events := h.recordedEvents()
	assert.Equal(t, "sentinel "+podcontract.RestoreCompleteFile, events[0], "nothing checks the file before the release")
	assert.Equal(t, []string{"sentinel " + podcontract.RestoreFailedFile, "kill"}, events[len(events)-2:],
		"a restored process whose application was never notified is stopped")
}

func TestPostRestoreFileWhoseDirectoryTheRestoreBringsSucceeds(t *testing.T) {
	h := newPostRestoreHarness(t, filePostRestoreSpec("/app-data/released"))
	h.restoreDir = "/app-data"

	require.NoError(t, h.run(false))
	assert.True(t, h.hasFile("/app-data/released"))
}

func TestPostRestoreRefusedTargetsFailThePreflight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action *snapshotv1alpha1.PostRestoreAction
		mutate func(*corev1.Pod)
	}{
		{"Snapshot's own control file", filePostRestoreSpec("/snapshot-control/" + podcontract.RestoreCompleteFile), func(pod *corev1.Pod) {
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name: podcontract.SnapshotControlVolumeName, MountPath: podcontract.SnapshotControlMountPath,
			})
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: podcontract.SnapshotControlVolumeName,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		}},
		{"HTTP on a port name the destination doesn't have", httpPostRestoreSpecOn(intstr.FromString("metrics")), func(*corev1.Pod) {}},
		{"HTTP path that doesn't parse", httpPostRestoreSpecWithPath("/%zz"), func(*corev1.Pod) {}},
		{"neither file nor httpPost", &snapshotv1alpha1.PostRestoreAction{PeriodSeconds: 1, TimeoutSeconds: 1}, func(*corev1.Pod) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPostRestoreHarness(t, nil)
			tc.mutate(h.pod)
			mappings := []podcontract.ContainerMapping{{Source: "main", Destination: "main"}}

			// The restore preflight runs it, so a refused action restores nothing.
			handler, _, err := planPostRestore(h.pod, mappings, tc.action, h.w.hookExec, h.w.hookHTTP)

			require.Error(t, err)
			assert.Nil(t, handler)
			assert.Equal(t, executor.StepCheckPostRestore, executor.StepOf(err))
		})
	}
}

func TestPostRestoreHTTPRetriesUntilAccepted(t *testing.T) {
	action := httpPostRestoreSpec()
	// Not 1, the default for an unset period, so a lost period shows.
	action.PeriodSeconds = 3
	h := newPostRestoreHarness(t, action)
	h.httpResults = []int{http.StatusNotFound, 0, http.StatusOK}

	require.NoError(t, h.run(false))

	assert.Equal(t, []string{"sentinel " + podcontract.RestoreCompleteFile, "POST", "POST", "POST"}, h.recordedEvents())
	assert.Equal(t, []time.Duration{3 * time.Second, 3 * time.Second}, h.waitPeriods,
		"periodSeconds is the wait after each failed attempt")
	assert.Equal(t, "http://10.0.0.9:8000/checkpoint/resume", h.httpURL)
	assert.InDelta(t, float64(4*time.Second), float64(h.httpTimeout), float64(time.Second), "the action's timeout bounds each attempt")
}

func TestPostRestoreHTTPResolvesANamedPortOnTheDestination(t *testing.T) {
	h := newPostRestoreHarness(t, httpPostRestoreSpecOn(intstr.FromString("hooks")))

	require.NoError(t, h.run(false))

	assert.Equal(t, "http://10.0.0.9:8000/checkpoint/resume", h.httpURL)
}

func TestPostRestoreThatNeverSucceedsFailsTheRestore(t *testing.T) {
	h := newPostRestoreHarness(t, httpPostRestoreSpec())
	h.httpResults = []int{0}

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
		{"file not created yet", filePostRestoreSpec(postRestoreMarker), false, []string{"marker " + postRestoreMarker}},
		{"file created before the restart", filePostRestoreSpec(postRestoreMarker), true, nil},
		{"HTTP", httpPostRestoreSpec(), false, []string{"POST"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPostRestoreHarness(t, tc.action)
			recoveringPod(h)
			if tc.marker {
				h.addFile(postRestoreMarker)
			}

			require.NoError(t, h.run(true))

			assert.Equal(t, 0, h.restores, "CRIU must never run again")
			assert.Equal(t, tc.want, h.recordedEvents())
		})
	}
}

func TestPostRestoreRecoveryThatNeverSucceedsFailsTheRestore(t *testing.T) {
	h := newPostRestoreHarness(t, httpPostRestoreSpec())
	recoveringPod(h)
	h.httpResults = []int{0}

	err := h.run(true)

	require.Error(t, err)
	assert.Equal(t, executor.StepPostRestore, executor.StepOf(err))
	assert.Equal(t, 0, h.restores)
	events := h.recordedEvents()
	assert.Equal(t, []string{"sentinel " + podcontract.RestoreFailedFile, "kill"}, events[len(events)-2:])
}

func TestPostRestoreLeftForTheNextAgentWhenTheAgentStops(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovering=%v", recovering), func(t *testing.T) {
			h := newPostRestoreHarness(t, httpPostRestoreSpec())
			if recovering {
				recoveringPod(h)
			}
			h.httpResults = []int{0}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			// The agent shuts down while the action waits to retry.
			h.w.postRestoreWaitFn = func(waitCtx context.Context, _ time.Duration) error {
				stop()
				return waitCtx.Err()
			}

			err := h.w.runRestore(ctx, h.pod, h.plan, "main", "ctr-abc", time.Now(), recovering)

			require.ErrorIs(t, err, errRestoreInterrupted)
			events := h.recordedEvents()
			assert.NotContains(t, events, "sentinel "+podcontract.RestoreFailedFile, "the next agent must be able to resume")
			assert.NotContains(t, events, "kill", "the restored process keeps running")
		})
	}
}

func TestNoPostRestoreLeavesRecoveryAsBefore(t *testing.T) {
	h := newPostRestoreHarness(t, nil)
	recoveringPod(h)

	require.NoError(t, h.run(true))
	assert.Empty(t, h.recordedEvents())
	assert.Equal(t, 0, h.restores)
}
