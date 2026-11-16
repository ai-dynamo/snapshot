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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// retryStatusHarness publishes the retry status of a Pod restoring two
// destination containers.
type retryStatusHarness struct {
	w       *NodeController
	pod     *corev1.Pod
	volume  *fakeControlVolume
	plan    *restorePlan
	results []restoreResult
}

func newRetryStatusHarness(t *testing.T) *retryStatusHarness {
	t.Helper()
	pod := multiRestorePod()
	pod.Annotations[podcontract.RestoreRetryLimitAnnotation] = "2"
	w := makeTestController(t, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 4242, podUID: "host-uid"}
	return &retryStatusHarness{
		w:      w,
		pod:    pod,
		volume: newFakeControlVolume(w),
		plan: &restorePlan{
			artifact: &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"},
			mappings: []podcontract.ContainerMapping{{Source: "main", Destination: "engine-0"}, {Source: "main", Destination: "engine-1"}},
		},
		results: []restoreResult{
			{destination: "engine-0", state: restoreResultPending, containerID: "engine-0-id"},
			{destination: "engine-1", state: restoreResultPending, containerID: "engine-1-id"},
		},
	}
}

func failedRecord(attempt, limit int, cause string) restoreAttempts {
	return restoreAttempts{Limit: limit, Attempt: attempt, ContainerID: "ctr", LastFailure: &podcontract.FailureSummary{Step: "restoring with CRIU", Cause: cause}}
}

func (h *retryStatusHarness) publish() {
	h.w.publishRetryStatus(context.Background(), h.pod, h.plan, h.results)
}

// applies returns the server-side applies to the Pod itself, not its status.
func (h *retryStatusHarness) applies() []clientgotesting.PatchAction {
	var applies []clientgotesting.PatchAction
	for _, action := range h.w.clientset.(*fake.Clientset).Actions() {
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

func (h *retryStatusHarness) published(t *testing.T) podcontract.RestoreRetryStatus {
	t.Helper()
	live, err := h.w.clientset.CoreV1().Pods(h.pod.Namespace).Get(context.Background(), h.pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	status, found, err := podcontract.RestoreRetryStatusFromAnnotations(live.Annotations)
	require.NoError(t, err)
	require.True(t, found, "no retry status on the Pod")
	assert.Equal(t, "snapshot-a", live.Annotations[podcontract.RestoreFromAnnotation], "the apply dropped another annotation")
	return status
}

func appliedRetryStatus(t *testing.T, action clientgotesting.PatchAction) (string, podcontract.RestoreRetryStatus) {
	t.Helper()
	var patch struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(action.GetPatch(), &patch))
	status, found, err := podcontract.RestoreRetryStatusFromAnnotations(patch.Metadata.Annotations)
	require.NoError(t, err)
	require.True(t, found, "the apply carries no retry status")
	return patch.Metadata.Annotations[podcontract.RestoreRetryStatusAnnotation], status
}

func TestPublishRetryStatusWritesEveryDestinationOnce(t *testing.T) {
	h := newRetryStatusHarness(t)
	h.volume.put(t, "host-uid", "engine-0", failedRecord(2, 3, "criu restore failed"))
	h.volume.put(t, "host-uid", "engine-1", failedRecord(1, 3, "cuda-checkpoint failed"))

	h.publish()

	applies := h.applies()
	require.Len(t, applies, 1)
	assert.Equal(t, restoreStatusFieldManager, fieldManager(t, applies[0]))
	var patch struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(applies[0].GetPatch(), &patch))
	assert.Len(t, patch.Metadata.Annotations, 1, "the apply owns more than the retry status")
	assert.Equal(t, podcontract.RestoreRetryStatus{
		"engine-0": {Attempt: 2, RetryLimit: 3, LastFailure: podcontract.FailureSummary{Step: "restoring with CRIU", Cause: "criu restore failed"}},
		"engine-1": {Attempt: 1, RetryLimit: 3, LastFailure: podcontract.FailureSummary{Step: "restoring with CRIU", Cause: "cuda-checkpoint failed"}},
	}, h.published(t))
}

func TestPublishRetryStatusLeavesOutADestinationWithoutAFailure(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *retryStatusHarness){
		"no record": func(*testing.T, *retryStatusHarness) {},
		"a record, no failure": func(t *testing.T, h *retryStatusHarness) {
			h.volume.put(t, "host-uid", "engine-1", restoreAttempts{Attempt: 1, ContainerID: "ctr"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRetryStatusHarness(t)
			h.volume.put(t, "host-uid", "engine-0", failedRecord(1, 2, "criu restore failed"))
			setup(t, h)

			h.publish()

			status := h.published(t)
			assert.Contains(t, status, "engine-0")
			assert.NotContains(t, status, "engine-1")
		})
	}
}

func TestPublishRetryStatusWritesNothing(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *retryStatusHarness){
		"without a failure": func(t *testing.T, h *retryStatusHarness) {
			h.volume.put(t, "host-uid", "engine-0", restoreAttempts{Attempt: 1, ContainerID: "ctr"})
		},
		"when the Pod already says it": func(t *testing.T, h *retryStatusHarness) {
			h.volume.put(t, "host-uid", "engine-0", failedRecord(1, 2, "criu restore failed"))
			value, err := podcontract.FormatRestoreRetryStatus(podcontract.RestoreRetryStatus{
				"engine-0": {Attempt: 1, RetryLimit: 2, LastFailure: podcontract.FailureSummary{Step: "restoring with CRIU", Cause: "criu restore failed"}},
			})
			require.NoError(t, err)
			h.pod.Annotations[podcontract.RestoreRetryStatusAnnotation] = value
		},
		"when the pass found no container": func(t *testing.T, h *retryStatusHarness) {
			for _, uid := range []string{"host-uid", "restore-pod-uid"} {
				h.volume.put(t, uid, "engine-0", failedRecord(1, 2, "criu restore failed"))
			}
			for i := range h.results {
				h.results[i].containerID = ""
			}
		},
		"when a record cannot be read": func(t *testing.T, h *retryStatusHarness) {
			h.volume.put(t, "host-uid", "engine-0", failedRecord(1, 2, "criu restore failed"))
			h.volume.put(t, "host-uid", "engine-1", []byte("attempt=1"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRetryStatusHarness(t)
			setup(t, h)

			h.publish()

			assert.Empty(t, h.applies())
		})
	}
}

// A restore that starts over removes the status an earlier restore of the Pod
// published, once no destination's record holds a failure.
func TestPublishRetryStatusRemovesTheStatusOfAnEarlierRestore(t *testing.T) {
	h := newRetryStatusHarness(t)
	h.volume.put(t, "host-uid", "engine-0", failedRecord(1, 2, "criu restore failed"))
	h.publish()
	require.Contains(t, h.published(t), "engine-0")
	live, err := h.w.clientset.CoreV1().Pods(h.pod.Namespace).Get(context.Background(), h.pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	h.pod = live
	h.volume.put(t, "host-uid", "engine-0", restoreAttempts{Attempt: 1, ContainerID: "ctr"})

	h.publish()

	live, err = h.w.clientset.CoreV1().Pods(h.pod.Namespace).Get(context.Background(), h.pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, live.Annotations, podcontract.RestoreRetryStatusAnnotation)
	assert.Equal(t, "snapshot-a", live.Annotations[podcontract.RestoreFromAnnotation], "the apply dropped another annotation")
	require.Len(t, h.applies(), 2)
	h.pod = live
	h.publish()
	assert.Len(t, h.applies(), 2, "a Pod without the status got another apply")
}

// A restore without retries keeps no record, so the pass reads none.
func TestPublishRetryStatusReadsNothingWithoutRetries(t *testing.T) {
	h := newRetryStatusHarness(t)
	h.pod.Annotations[podcontract.RestoreRetryLimitAnnotation] = "0"

	h.publish()

	assert.Empty(t, h.volume.reads)
	assert.Empty(t, h.applies())
}

func TestPublishRetryStatusReadsUnderThePodsNodeUID(t *testing.T) {
	for name, tc := range map[string]struct {
		runtime       *fakeRuntime
		onlyTheSecond bool
		uid           string
	}{
		"the runtime names it":                     {runtime: &fakeRuntime{podUID: "host-uid"}, uid: "host-uid"},
		"the runtime cannot name it":               {runtime: &fakeRuntime{}, uid: "restore-pod-uid"},
		"the pass found only the second container": {runtime: &fakeRuntime{podUID: "host-uid"}, onlyTheSecond: true, uid: "host-uid"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRetryStatusHarness(t)
			h.w.runtime = tc.runtime
			if tc.onlyTheSecond {
				h.results[0].containerID = ""
			}
			h.volume.put(t, tc.uid, "engine-0", failedRecord(1, 2, "criu restore failed"))

			h.publish()

			assert.Contains(t, h.published(t), "engine-0")
		})
	}
}

func TestRestorePodNodeUID(t *testing.T) {
	for name, tc := range map[string]struct {
		runtime      *fakeRuntime
		containerIDs []string
		wantUID      string
		wantOK       bool
	}{
		"no container found":         {runtime: &fakeRuntime{podUID: "host-uid"}, containerIDs: []string{"", ""}},
		"a UID from the runtime":     {runtime: &fakeRuntime{podUID: "host-uid"}, containerIDs: []string{"", "engine-1-id"}, wantUID: "host-uid", wantOK: true},
		"the runtime cannot name it": {runtime: &fakeRuntime{}, containerIDs: []string{"engine-0-id", "engine-1-id"}, wantUID: "restore-pod-uid", wantOK: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRetryStatusHarness(t)
			h.w.runtime = tc.runtime
			for i, containerID := range tc.containerIDs {
				h.results[i].containerID = containerID
			}

			uid, ok := h.w.restorePodNodeUID(context.Background(), h.pod, h.results)

			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantUID, uid)
			}
		})
	}
}

func TestPublishRetryStatusReportsAFailedWrite(t *testing.T) {
	h := newRetryStatusHarness(t)
	h.volume.put(t, "host-uid", "engine-0", failedRecord(1, 2, "criu restore failed"))
	h.w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "" {
			return false, nil, nil
		}
		return true, nil, errors.New("apiserver unavailable")
	})

	h.publish()

	event := eventForReason(h.w.clientset.(*fake.Clientset), restoreStatusUpdateFailedReason)
	require.NotNil(t, event)
	assert.Equal(t, "Failed to record the restore retry status: apiserver unavailable", event.Message)
}
