// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// containerPIDs resolves each container to its own placeholder PID, so a test
// can tell the destinations' control directories apart.
type containerPIDs struct {
	*fakeRuntime
	pids map[string]int
}

func (r containerPIDs) ResolveContainer(_ context.Context, id string) (int, *specs.Spec, error) {
	if pid, ok := r.pids[id]; ok {
		return pid, &specs.Spec{}, nil
	}
	return 0, nil, fmt.Errorf("container %s is not running", id)
}

func withRestoredCondition(pod *corev1.Pod, status corev1.ConditionStatus, reason string) *corev1.Pod {
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodConditionType(podcontract.RestoredCondition),
		Status: status,
		Reason: reason,
	})
	return pod
}

func cacheRestorePods(w *NodeController, pods ...*corev1.Pod) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, pod := range pods {
		_ = indexer.Add(pod)
	}
	w.restorePodLister = corev1listers.NewPodLister(indexer)
}

// identity is the Pod as the NRI plugin knows it.
func identity(pod *corev1.Pod) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID}}
}

func TestRestoreEnded(t *testing.T) {
	annotations := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}
	for name, tc := range map[string]struct {
		pod  func() *corev1.Pod
		want bool
	}{
		"restore succeeded": {
			pod: func() *corev1.Pod {
				return withRestoredCondition(restorePod(annotations), corev1.ConditionTrue, podcontract.RestoreReasonSucceeded)
			},
			want: true,
		},
		"restore failed": {
			pod: func() *corev1.Pod {
				return withRestoredCondition(restorePod(annotations), corev1.ConditionFalse, podcontract.RestoreReasonFailed)
			},
			want: true,
		},
		"restore partially succeeded": {
			pod: func() *corev1.Pod {
				return withRestoredCondition(restorePod(annotations), corev1.ConditionFalse, podcontract.RestoreReasonPartiallySucceeded)
			},
			want: true,
		},
		"restore refused": {
			pod: func() *corev1.Pod {
				return withRestoredCondition(restorePod(annotations), corev1.ConditionFalse, podcontract.RestoreReasonIncompatible)
			},
			want: true,
		},
		"restore in progress": {
			pod: func() *corev1.Pod {
				return withRestoredCondition(restorePod(annotations), corev1.ConditionFalse, podcontract.RestoreReasonInProgress)
			},
		},
		"restore not started": {
			pod: func() *corev1.Pod { return restorePod(annotations) },
		},
		"Pod being deleted": {
			pod: func() *corev1.Pod {
				pod := withRestoredCondition(restorePod(annotations), corev1.ConditionTrue, podcontract.RestoreReasonSucceeded)
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				return pod
			},
		},
		"another Pod under the name": {
			pod: func() *corev1.Pod {
				pod := withRestoredCondition(restorePod(annotations), corev1.ConditionTrue, podcontract.RestoreReasonSucceeded)
				pod.UID = "other-uid"
				return pod
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := makeTestController(t, nil)
			cacheRestorePods(w, tc.pod())

			assert.Equal(t, tc.want, w.RestoreEnded(identity(restorePod(annotations))))
		})
	}

	t.Run("Pod not in the cache", func(t *testing.T) {
		w := makeTestController(t, nil)
		cacheRestorePods(w)

		assert.False(t, w.RestoreEnded(identity(restorePod(annotations))))
	})
}

func TestRestoreAgainSetsTheRestoreBackInProgress(t *testing.T) {
	pod := withRestoredCondition(restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}),
		corev1.ConditionTrue, podcontract.RestoreReasonSucceeded)
	w := makeTestController(t, pod)
	cacheRestorePods(w, pod)

	require.NoError(t, w.RestoreAgain(context.Background(), identity(pod), "main"))

	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"reason":"RestoreInProgress"`)
	assert.Contains(t, payload, "Destination main started again; restoring it again from PodSnapshot snapshot-a")
	event := eventForReason(w.clientset.(*fake.Clientset), restoreRepeatedReason)
	require.NotNil(t, event)
	assert.Equal(t, corev1.EventTypeNormal, event.Type)
	assert.Equal(t, 1, w.restoreQueue.Len())
	_, restarted := w.restartedRestoreContainers.Load(restoreContainerKey(pod.UID, "main"))
	assert.True(t, restarted)
	assert.Equal(t, podcontract.RestoreReasonSucceeded, restoredPodCondition(pod).Reason, "the cached Pod is not changed")
}

// An error fails the container's creation, so kubelet creates it again.
func TestRestoreAgainFails(t *testing.T) {
	annotations := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}

	t.Run("another Pod under the name", func(t *testing.T) {
		pod := withRestoredCondition(restorePod(annotations), corev1.ConditionTrue, podcontract.RestoreReasonSucceeded)
		w := makeTestController(t, pod)
		cacheRestorePods(w, pod)
		other := identity(pod)
		other.UID = "other-uid"

		require.Error(t, w.RestoreAgain(context.Background(), other, "main"))
		assert.False(t, hasPodStatusApply(w))
	})

	t.Run("status write fails", func(t *testing.T) {
		pod := withRestoredCondition(restorePod(annotations), corev1.ConditionTrue, podcontract.RestoreReasonSucceeded)
		w := makeTestController(t, pod)
		cacheRestorePods(w, pod)
		w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unavailable")
		})

		require.Error(t, w.RestoreAgain(context.Background(), identity(pod), "main"))
		assert.False(t, sawEventReason(w.clientset.(*fake.Clientset), restoreRepeatedReason))
	})
}

// Until the cache shows the new restore, the agent must not act on the ended
// one: that would mark it handled, and the new restore would never run.
func TestRestoreAgainWaitsForTheCache(t *testing.T) {
	pod := withRestoredCondition(restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}),
		corev1.ConditionTrue, podcontract.RestoreReasonSucceeded)
	w := makeTestController(t, pod)
	now := time.Now()
	w.nowFn = func() time.Time { return now }
	cacheRestorePods(w, pod)
	require.NoError(t, w.RestoreAgain(context.Background(), identity(pod), "main"))

	processQueuedRestorePod(t, w, pod)

	assert.False(t, sawEventReason(w.clientset.(*fake.Clientset), restoreAlreadyCompletedReason))
	assert.False(t, w.restoreHandled(pod))

	now = now.Add(restoreAgainCacheWait)
	processQueuedRestorePod(t, w, pod)

	assert.True(t, sawEventReason(w.clientset.(*fake.Clientset), restoreAlreadyCompletedReason),
		"a status write that never showed up stops holding the Pod back")
	_, waiting := w.restoresStartedAgain.Load(string(pod.UID))
	assert.False(t, waiting)
}

// A restore Pod whose restore succeeded has a destination that starts again.
// The next pass restores that destination into its new container, and keeps
// the other destination's restore.
func TestRestoreAgainRestoresTheDestinationThatStartedAgain(t *testing.T) {
	pod := withRestoredCondition(multiRestorePod(), corev1.ConditionTrue, podcontract.RestoreReasonSucceeded)
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	w.runtime = containerPIDs{fakeRuntime: &fakeRuntime{}, pids: map[string]int{"engine-0-id": 100, "engine-1-new": 201}}
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	var mu sync.Mutex
	complete := map[int]bool{100: true}
	w.controlSentinelExistsFn = func(pid int, name string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return name == podcontract.RestoreCompleteFile && complete[pid], nil
	}
	w.writeControlSentinelFn = func(pid int, name string, _ []byte) error {
		mu.Lock()
		defer mu.Unlock()
		complete[pid] = name == podcontract.RestoreCompleteFile
		return nil
	}
	var restored []string
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		mu.Lock()
		defer mu.Unlock()
		restored = append(restored, req.ContainerID)
		return executor.RestoreResult{PlaceholderHostPID: 201, RestoredPID: 1}, nil
	}
	w.markRestoreHandled(pod)
	cacheRestorePods(w, pod)

	require.NoError(t, w.RestoreAgain(context.Background(), identity(pod), "engine-1"))
	w.RestoreContainerStarted(client.ObjectKeyFromObject(pod), pod.UID, "engine-1", "engine-1-new")
	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	processQueuedRestorePod(t, w, live)

	assert.Equal(t, []string{"engine-1-new"}, restored,
		"only the destination that started again is restored, into its new container and not the one its status still names")
	live, err = w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	condition := restoredPodCondition(live)
	require.NotNil(t, condition)
	assert.Equal(t, corev1.ConditionTrue, condition.Status)
	assert.Equal(t, podcontract.RestoreReasonSucceeded, condition.Reason)
	assert.True(t, w.restoreHandled(live))
	_, restarted := w.restartedRestoreContainers.Load(restoreContainerKey(pod.UID, "engine-1"))
	assert.False(t, restarted)
}

func TestResolveRestoreContainerIDSkipsTheStatusOfADestinationRestoredAgain(t *testing.T) {
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	podKey := client.ObjectKeyFromObject(pod)
	w := makeTestController(t, pod)
	w.restartedRestoreContainers.Store(restoreContainerKey(pod.UID, "main"), struct{}{})
	w.RestoreContainerStarted(podKey, pod.UID, "main", "nri-new")

	containerID, _ := w.resolveRestoreContainerID(canceledContext(), pod, "main", podKey.String())

	assert.Equal(t, "nri-new", containerID)

	w.forgetRestorePod(pod)
	_, restarted := w.restartedRestoreContainers.Load(restoreContainerKey(pod.UID, "main"))
	assert.False(t, restarted, "forgotten with the Pod")
	containerID, _ = w.resolveRestoreContainerID(canceledContext(), pod, "main", podKey.String())
	assert.Equal(t, testContainerID, containerID)
}
