// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// writeQueuedRetryStatus makes the retry status writes queued so far, as the
// agent's worker does outside the restore.
func writeQueuedRetryStatus(w *NodeController) {
	for w.retryStatusQueue.Len() > 0 {
		w.processRetryStatusQueueItem(context.Background())
	}
}

// retryStatusApplies returns the server-side applies to the Pod itself, not
// its status.
func retryStatusApplies(w *NodeController) []clientgotesting.PatchAction {
	var applies []clientgotesting.PatchAction
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		patch, ok := action.(clientgotesting.PatchAction)
		if ok && patch.GetSubresource() == "" && patch.GetPatchType() == ktypes.ApplyPatchType {
			applies = append(applies, patch)
		}
	}
	return applies
}

func fieldManager(t *testing.T, action clientgotesting.PatchAction) string {
	t.Helper()
	withOptions, ok := action.(interface{ GetPatchOptions() metav1.PatchOptions })
	require.True(t, ok, "%T carries no patch options", action)
	return withOptions.GetPatchOptions().FieldManager
}

// appliedMetadata returns the metadata one apply sets.
func appliedMetadata(t *testing.T, action clientgotesting.PatchAction) metav1.ObjectMeta {
	t.Helper()
	var patch struct {
		Metadata metav1.ObjectMeta `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(action.GetPatch(), &patch))
	return patch.Metadata
}

// publishedRetryStatus returns the retry status on the Pod in the API, and
// false when it has none.
func publishedRetryStatus(t *testing.T, w *NodeController, pod *corev1.Pod) (podcontract.RestoreRetryStatus, bool) {
	t.Helper()
	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "snapshot-a", live.Annotations[podcontract.RestoreFromAnnotation], "the apply dropped another annotation")
	status, found, err := podcontract.RestoreRetryStatusFromAnnotations(live.Annotations)
	require.NoError(t, err)
	return status, found
}

func retryStatusPlan() *restorePlan {
	return &restorePlan{artifact: &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"}}
}

func newRetryStatusController(t *testing.T) (*NodeController, *corev1.Pod) {
	t.Helper()
	pod := multiRestorePod()
	pod.Annotations[podcontract.RestoreRetryLimitAnnotation] = "2"
	return makeTestController(t, pod), pod
}

// The destinations of a Pod restore in parallel. Every update queued before a
// write goes out in that one write, with every destination's entry.
func TestWriteRetryStatusWritesEveryDestinationOnce(t *testing.T) {
	w, pod := newRetryStatusController(t)
	putAttempts(w, pod.UID, "engine-0", restoreAttempts{Limit: 2, Attempt: 2, ContainerID: "engine-0-id"})
	w.queueRetryStatus(pod)
	putAttempts(w, pod.UID, "engine-1", restoreAttempts{Limit: 2, Attempt: 1, ContainerID: "engine-1-id"})
	w.queueRetryStatus(pod)
	putAttempts(w, "another-pod-uid", "engine-0", restoreAttempts{Limit: 2, Attempt: 1, ContainerID: "other-id"})

	writeQueuedRetryStatus(w)

	applies := retryStatusApplies(w)
	require.Len(t, applies, 1)
	assert.Equal(t, restoreStatusFieldManager, fieldManager(t, applies[0]))
	metadata := appliedMetadata(t, applies[0])
	assert.Equal(t, pod.UID, metadata.UID, "the apply could land on a new Pod of the same name")
	assert.Len(t, metadata.Annotations, 1, "the apply owns more than the retry status")
	status, found := publishedRetryStatus(t, w, pod)
	require.True(t, found)
	assert.Equal(t, podcontract.RestoreRetryStatus{
		"engine-0": {Attempt: 2, RetryLimit: 2, ContainerID: "engine-0-id", PodUID: pod.UID},
		"engine-1": {Attempt: 1, RetryLimit: 2, ContainerID: "engine-1-id", PodUID: pod.UID},
	}, status)
}

func TestWriteRetryStatusSkipsAValueAlreadyWritten(t *testing.T) {
	w, pod := newRetryStatusController(t)
	putAttempts(w, pod.UID, "engine-0", restoreAttempts{Limit: 2, Attempt: 1, ContainerID: "engine-0-id"})
	w.queueRetryStatus(pod)
	writeQueuedRetryStatus(w)

	w.queueRetryStatus(pod)
	writeQueuedRetryStatus(w)

	assert.Len(t, retryStatusApplies(w), 1)
}

func TestWriteRetryStatusRemovesTheAnnotationWithNoRecordsLeft(t *testing.T) {
	w, pod := newRetryStatusController(t)
	putAttempts(w, pod.UID, "engine-0", restoreAttempts{Limit: 2, Attempt: 1, ContainerID: "engine-0-id"})
	w.queueRetryStatus(pod)
	writeQueuedRetryStatus(w)

	w.restoreAttemptRecords.Delete(restoreContainerKey(pod.UID, "engine-0"))
	w.queueRetryStatus(pod)
	writeQueuedRetryStatus(w)

	applies := retryStatusApplies(w)
	require.Len(t, applies, 2)
	assert.Empty(t, appliedMetadata(t, applies[1]).Annotations)
	_, found := publishedRetryStatus(t, w, pod)
	assert.False(t, found)
}

// A restarted agent removes an entry an earlier agent wrote once it forgets
// the record, as after a refusal.
func TestWriteRetryStatusRemovesAnEntryAnEarlierAgentWrote(t *testing.T) {
	w, pod := newRetryStatusController(t)
	leaveRetryStatus(t, pod, podcontract.RestoreRetryStatus{
		"engine-0": {Attempt: 1, RetryLimit: 2, ContainerID: "engine-0-id", PodUID: pod.UID},
	})
	op := w.newRestoreOperation(pod, retryStatusPlan(), "engine-0", "engine-0-id", w.nowFn())
	_, found, err := op.loadAttempts()
	require.NoError(t, err)
	require.True(t, found)

	op.forgetAttempts()
	writeQueuedRetryStatus(w)

	applies := retryStatusApplies(w)
	require.Len(t, applies, 1)
	assert.Empty(t, appliedMetadata(t, applies[0]).Annotations)
}

func TestWriteRetryStatusWritesNothingForAPodWithNoRecords(t *testing.T) {
	w, pod := newRetryStatusController(t)

	w.queueRetryStatus(pod)
	writeQueuedRetryStatus(w)

	assert.Empty(t, retryStatusApplies(w))
}

func TestWriteRetryStatusRetriesAFailedWriteThenReportsIt(t *testing.T) {
	w, pod := newRetryStatusController(t)
	putAttempts(w, pod.UID, "engine-0", restoreAttempts{Limit: 2, Attempt: 1, ContainerID: "engine-0-id"})
	writes := 0
	w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "" {
			return false, nil, nil
		}
		writes++
		return true, nil, errors.New("apiserver unavailable")
	})

	w.queueRetryStatus(pod)
	for range retryStatusWriteAttempts {
		w.processRetryStatusQueueItem(context.Background())
	}

	assert.Equal(t, retryStatusWriteAttempts, writes)
	assert.Zero(t, w.retryStatusQueue.Len())
	assert.Zero(t, w.retryStatusQueue.NumRequeues(retryStatusKey{namespace: pod.Namespace, name: pod.Name, uid: pod.UID}))
	events := eventsForReason(w.clientset.(*fake.Clientset), restoreStatusUpdateFailedReason)
	require.Len(t, events, 1)
	assert.Equal(t, "Failed to record the restore retry status: apiserver unavailable", events[0].Message)
	assert.Equal(t, pod.UID, events[0].InvolvedObject.UID)
}

func TestWriteRetryStatusDropsAPodThatIsGone(t *testing.T) {
	w, pod := newRetryStatusController(t)
	putAttempts(w, pod.UID, "engine-0", restoreAttempts{Limit: 2, Attempt: 1, ContainerID: "engine-0-id"})
	w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pod.Name)
	})

	w.queueRetryStatus(pod)
	writeQueuedRetryStatus(w)

	assert.Zero(t, w.retryStatusQueue.Len())
	assert.Empty(t, eventsForReason(w.clientset.(*fake.Clientset), restoreStatusUpdateFailedReason))
}

func TestWriteRetryStatusGivesUpOnShutdown(t *testing.T) {
	w, pod := newRetryStatusController(t)
	putAttempts(w, pod.UID, "engine-0", restoreAttempts{Limit: 2, Attempt: 1, ContainerID: "engine-0-id"})
	ctx, cancel := context.WithCancel(context.Background())
	w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
		cancel()
		return true, nil, context.Canceled
	})

	w.queueRetryStatus(pod)
	w.processRetryStatusQueueItem(ctx)

	assert.Zero(t, w.retryStatusQueue.Len())
	assert.Empty(t, eventsForReason(w.clientset.(*fake.Clientset), restoreStatusUpdateFailedReason))
}

func TestRunRetryStatusQueueStopsWhenTheQueueShutsDown(t *testing.T) {
	w, _ := newRetryStatusController(t)
	done := make(chan struct{})
	go func() {
		w.runRetryStatusQueue(context.Background())
		close(done)
	}()

	w.retryStatusQueue.ShutDown()

	<-done
}

// A deleted Pod takes its records with it, so a new Pod of the same name
// starts a new restore.
func TestForgetRestorePodForgetsItsAttempts(t *testing.T) {
	w, pod := newRetryStatusController(t)
	putAttempts(w, pod.UID, "engine-0", restoreAttempts{Limit: 2, Attempt: 1, ContainerID: "engine-0-id"})
	putAttempts(w, "another-pod-uid", "engine-0", restoreAttempts{Limit: 2, Attempt: 1, ContainerID: "other-id"})
	op := w.newRestoreOperation(pod, retryStatusPlan(), "engine-0", "engine-0-id", w.nowFn())
	_, _, err := op.loadAttempts()
	require.NoError(t, err)
	w.retryStatusWritten.Store(pod.UID, "written")

	w.forgetRestorePod(pod)

	assert.Nil(t, savedAttempts(w, pod.UID, "engine-0"))
	assert.NotNil(t, savedAttempts(w, "another-pod-uid", "engine-0"))
	_, seeded := w.attemptSeeds.Load(pod.UID)
	assert.False(t, seeded)
	_, written := w.retryStatusWritten.Load(pod.UID)
	assert.False(t, written)
}
