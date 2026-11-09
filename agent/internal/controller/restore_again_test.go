// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// restoredPod is a restore Pod into engine-0 and engine-1 whose restore
// succeeded at restoredAt, with each container started at its time in
// started.
func restoredPod(limit string, restoredAt time.Time, reason string, started map[string]time.Time) *corev1.Pod {
	pod := multiRestorePod()
	if limit != "" {
		pod.Annotations[podcontract.RestoreRetryLimitAnnotation] = limit
	}
	restored := corev1.ConditionFalse
	if reason == podcontract.RestoreReasonSucceeded {
		restored = corev1.ConditionTrue
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:               corev1.PodConditionType(podcontract.RestoredCondition),
		Status:             restored,
		Reason:             reason,
		LastTransitionTime: metav1.NewTime(restoredAt),
	})
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		status.State.Running.StartedAt = metav1.NewTime(started[status.Name])
	}
	return pod
}

func TestRestartedDestinations(t *testing.T) {
	restoredAt := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	before, after := restoredAt.Add(-time.Minute), restoredAt.Add(30*time.Second)
	complete := podcontract.RestoreCompleteFile
	for name, tc := range map[string]struct {
		limit         string
		reason        string
		started       map[string]time.Time
		outcomes      map[string][]string
		setup         func(*NodeController, *corev1.Pod)
		wantRestarted []string
		wantDone      []string
		wantRetry     bool
	}{
		"a container that started again": {
			limit:         "2",
			started:       map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes:      map[string][]string{"engine-0": {complete}},
			wantRestarted: []string{"engine-1"},
			wantDone:      []string{"engine-0"},
		},
		"both containers started again": {
			limit:         "2",
			started:       map[string]time.Time{"engine-0": after, "engine-1": after},
			wantRestarted: []string{"engine-0", "engine-1"},
		},
		"a container that started in the second the restore succeeded": {
			limit:         "2",
			started:       map[string]time.Time{"engine-0": before, "engine-1": restoredAt},
			outcomes:      map[string][]string{"engine-0": {complete}},
			wantRestarted: []string{"engine-1"},
			wantDone:      []string{"engine-0"},
		},
		"the agent's default limit": {
			started:       map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes:      map[string][]string{"engine-0": {complete}},
			setup:         func(w *NodeController, _ *corev1.Pod) { w.config.Restore.RetryLimit = ptr.To(1) },
			wantRestarted: []string{"engine-1"},
			wantDone:      []string{"engine-0"},
		},
		"no retry limit": {
			started:  map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes: map[string][]string{"engine-0": {complete}},
		},
		"a retry limit of 0": {
			limit:    "0",
			started:  map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes: map[string][]string{"engine-0": {complete}},
		},
		"without the restore proxy": {
			limit:    "2",
			started:  map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes: map[string][]string{"engine-0": {complete}},
			setup:    func(w *NodeController, _ *corev1.Pod) { w.withoutProxy = true },
		},
		"a restore that partly succeeded": {
			limit:    "2",
			reason:   podcontract.RestoreReasonPartiallySucceeded,
			started:  map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes: map[string][]string{"engine-0": {complete}},
		},
		"a Pod being deleted": {
			limit:    "2",
			started:  map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes: map[string][]string{"engine-0": {complete}},
			setup: func(_ *NodeController, pod *corev1.Pod) {
				pod.DeletionTimestamp = &metav1.Time{Time: after}
			},
		},
		"containers that started before the restore succeeded": {
			limit:    "2",
			started:  map[string]time.Time{"engine-0": before, "engine-1": before},
			outcomes: map[string][]string{"engine-0": {complete}},
		},
		"a container that keeps the outcome of the restore": {
			limit:    "2",
			started:  map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes: map[string][]string{"engine-0": {complete}, "engine-1": {complete}},
		},
		"a container whose restore failed": {
			limit:    "2",
			started:  map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes: map[string][]string{"engine-0": {complete}, "engine-1": {complete, podcontract.RestoreFailedFile}},
		},
		"a restarted container that holds restore-failed": {
			limit:    "2",
			started:  map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes: map[string][]string{"engine-0": {complete}, "engine-1": {podcontract.RestoreFailedFile}},
		},
		"a running container without an ID": {
			limit:   "2",
			started: map[string]time.Time{"engine-0": before, "engine-1": after},
			setup: func(_ *NodeController, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[1].ContainerID = ""
				pod.Annotations[podcontract.RestoreContainerMapAnnotation] = "main"
			},
		},
		"a sibling without the outcome of its restore": {
			limit:   "2",
			started: map[string]time.Time{"engine-0": before, "engine-1": after},
		},
		"a restarted container that is not running": {
			limit:    "2",
			started:  map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes: map[string][]string{"engine-0": {complete}},
			setup: func(_ *NodeController, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
			},
		},
		"the runtime cannot name the Pod's UID": {
			limit:     "2",
			started:   map[string]time.Time{"engine-0": before, "engine-1": after},
			outcomes:  map[string][]string{"engine-0": {complete}},
			setup:     func(w *NodeController, _ *corev1.Pod) { w.runtime = &fakeRuntime{} },
			wantRetry: true,
		},
		"an outcome that cannot be read": {
			limit:   "2",
			started: map[string]time.Time{"engine-0": before, "engine-1": after},
			setup: func(w *NodeController, _ *corev1.Pod) {
				w.podControlSentinelExistsFn = func(string, string, string) (bool, error) {
					return false, errors.New("restore-complete is not a regular file")
				}
			},
			wantRetry: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			reason := tc.reason
			if reason == "" {
				reason = podcontract.RestoreReasonSucceeded
			}
			pod := restoredPod(tc.limit, restoredAt, reason, tc.started)
			w := makeTestController(t, pod)
			w.runtime = &fakeRuntime{podUID: "host-uid"}
			volume := newFakeControlVolume(w)
			for destination, files := range tc.outcomes {
				for _, file := range files {
					volume.putSentinel("host-uid", destination, file)
				}
			}
			if tc.setup != nil {
				tc.setup(w, pod)
			}

			restart, retry := w.restartedDestinations(context.Background(), pod)

			assert.Equal(t, tc.wantRetry, retry)
			if tc.wantRestarted == nil {
				assert.Nil(t, restart)
				return
			}
			require.NotNil(t, restart)
			assert.Equal(t, "host-uid", restart.nodeUID)
			assert.Equal(t, tc.wantRestarted, restart.restarted)
			var done []string
			for _, result := range restart.done {
				assert.Equal(t, restoreResultSucceeded, result.state)
				done = append(done, result.destination)
			}
			assert.Equal(t, tc.wantDone, done)
		})
	}
}

// The pass after the reopen restores only the container that started again,
// from its first attempt, and reports its sibling's success without touching
// it.
func TestStartRestoreOverRestoresOnlyTheContainerThatStartedAgain(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	restoredAt := now.Add(-time.Hour)
	pod := restoredPod("2", restoredAt, podcontract.RestoreReasonSucceeded,
		map[string]time.Time{"engine-0": restoredAt.Add(-time.Minute), "engine-1": now})
	w := makeTestController(t, pod)
	w.config.Restore.TimeoutSeconds = 600
	w.runtime = &fakeRuntime{resolveContainerPID: 4242, podUID: "host-uid"}
	w.nowFn = func() time.Time { return now }
	volume := newFakeControlVolume(w)
	volume.putSentinel("host-uid", "engine-0", podcontract.RestoreCompleteFile)
	volume.put(t, "host-uid", "engine-0", restoreAttempts{Limit: 2, Attempt: 1, Deadline: restoredAt, ContainerID: "engine-0-id"})
	volume.put(t, "host-uid", "engine-1", failedRecord(2, 2, "criu restore failed"))
	w.finishedRestoreDestinations.Store(restoreContainerKey(pod.UID, "engine-1"), restoreResult{destination: "engine-1", state: restoreResultSucceeded})
	w.markRestoreHandled(pod)
	var mu sync.Mutex
	restores := map[string]int{}
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		mu.Lock()
		defer mu.Unlock()
		restores[req.DestinationContainerName]++
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 77}, nil
	}
	clientset := w.clientset.(*fake.Clientset)

	restart, retry := w.restartedDestinations(context.Background(), pod)
	require.False(t, retry)
	require.NotNil(t, restart)
	require.NoError(t, w.startRestoreOver(context.Background(), pod, restart))

	_, err := w.readPodControlFileFn("host-uid", "engine-1", podcontract.RestoreAttemptsFile, restoreAttemptsMaxBytes)
	require.Error(t, err, "the earlier restore's record would count its attempts against the new restore")
	assert.False(t, w.restoreHandled(pod))
	repeated := eventsForReason(clientset, restoreRepeatedReason)
	require.Len(t, repeated, 1)
	assert.Equal(t, "Destination engine-1 started again after its restore; restoring it again from PodSnapshot snapshot-a", repeated[0].Message)

	plan := &restorePlan{
		artifact: &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"},
		mappings: []podcontract.ContainerMapping{{Source: "main", Destination: "engine-0"}, {Source: "main", Destination: "engine-1"}},
	}
	require.False(t, w.restorePodContainers(context.Background(), pod, plan, "inference/restore-worker"))

	assert.Equal(t, map[string]int{"engine-1": 1}, restores)
	record := volume.record(t, "host-uid", "engine-1")
	assert.Equal(t, 1, record.Attempt)
	assert.Equal(t, 2, record.Limit)
	assert.True(t, now.Add(10*time.Minute).Equal(record.Deadline), "deadline = %s, want a new one", record.Deadline)
	assert.Equal(t, 1, volume.record(t, "host-uid", "engine-0").Attempt, "the sibling's record changed")
	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), podcontract.RestoreReasonSucceeded)
}

// A Pod whose container started again goes through preflight as a new restore
// instead of reporting its earlier success.
func TestProcessRestoreQueueItemReopensARestoreWhoseContainerStartedAgain(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	pod := restoredPod("2", now.Add(-time.Hour), podcontract.RestoreReasonSucceeded,
		map[string]time.Time{"engine-0": now.Add(-2 * time.Hour), "engine-1": now})
	w := makeTestController(t, pod)
	w.runtime = &fakeRuntime{podUID: "host-uid"}
	volume := newFakeControlVolume(w)
	volume.putSentinel("host-uid", "engine-0", podcontract.RestoreCompleteFile)
	clientset := w.clientset.(*fake.Clientset)

	processQueuedRestorePod(t, w, pod)

	assert.Len(t, eventsForReason(clientset, restoreRepeatedReason), 1)
	assert.False(t, sawEventReason(clientset, restoreAlreadyCompletedReason))
	assert.Empty(t, eventsForReason(clientset, restoreRequestedReason), "a reopened restore waits for its PodSnapshot like any other")
	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"SnapshotPending"`)
}

func TestProcessRestoreQueueItemLeavesARestoreThatSucceeded(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	pod := restoredPod("2", now.Add(-time.Hour), podcontract.RestoreReasonSucceeded,
		map[string]time.Time{"engine-0": now.Add(-2 * time.Hour), "engine-1": now.Add(-2 * time.Hour)})
	pod.Finalizers = []string{restorePodFinalizer}
	w := makeTestController(t, pod)
	w.runtime = &fakeRuntime{podUID: "host-uid"}
	newFakeControlVolume(w)
	clientset := w.clientset.(*fake.Clientset)

	processQueuedRestorePod(t, w, pod)

	assert.Empty(t, eventsForReason(clientset, restoreRepeatedReason))
	assert.True(t, sawEventReason(clientset, restoreAlreadyCompletedReason))
}

func TestRestoreAgain(t *testing.T) {
	succeeded := func(limit string) *corev1.Pod {
		return restoredPod(limit, time.Now(), podcontract.RestoreReasonSucceeded, nil)
	}
	for name, tc := range map[string]struct {
		cached       *corev1.Pod
		withoutProxy bool
		asked        func(*corev1.Pod) *corev1.Pod
		want         bool
	}{
		"a restore that succeeded":  {cached: succeeded("1"), want: true},
		"a retry limit of 0":        {cached: succeeded("0")},
		"no retry limit":            {cached: succeeded("")},
		"without the restore proxy": {cached: succeeded("1"), withoutProxy: true},
		"a restore that failed": {cached: func() *corev1.Pod {
			return restoredPod("1", time.Now(), podcontract.RestoreReasonFailed, nil)
		}()},
		"a restore in progress": {cached: func() *corev1.Pod {
			pod := succeeded("1")
			pod.Status.Conditions[len(pod.Status.Conditions)-1].Status = corev1.ConditionFalse
			pod.Status.Conditions[len(pod.Status.Conditions)-1].Reason = podcontract.RestoreReasonInProgress
			return pod
		}()},
		"another Pod under the same name": {cached: succeeded("1"), asked: func(pod *corev1.Pod) *corev1.Pod {
			pod.UID = "another-pod-uid"
			return pod
		}},
		"a Pod the cache does not hold": {},
	} {
		t.Run(name, func(t *testing.T) {
			w := makeTestController(t, nil)
			w.withoutProxy = tc.withoutProxy
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			asked := succeeded("1")
			if tc.cached != nil {
				require.NoError(t, indexer.Add(tc.cached))
				asked = tc.cached.DeepCopy()
			}
			w.restorePodLister = corev1listers.NewPodLister(indexer)
			if tc.asked != nil {
				asked = tc.asked(asked)
			}

			assert.Equal(t, tc.want, w.RestoreAgain(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: asked.Name, Namespace: asked.Namespace, UID: asked.UID, Annotations: asked.Annotations,
			}}))
		})
	}
}
