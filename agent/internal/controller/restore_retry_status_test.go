// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestFailureSummary(t *testing.T) {
	long := strings.Repeat("a", podcontract.FailureCauseMaxBytes-1) + "é and more"
	for name, tc := range map[string]struct {
		err  error
		want podcontract.FailureSummary
	}{
		"the cause beneath the step": {
			err:  &executor.StepError{Step: executor.StepPrepareRestore, Err: storeUnreachable},
			want: podcontract.FailureSummary{Step: "preparing PageBroker restore", Cause: "PageBroker STORAGE_UNAVAILABLE: store unreachable"},
		},
		"a code that names the checkpoint": {
			err:  untouched(pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_CORRUPT, "bad index")),
			want: podcontract.FailureSummary{Step: "preparing PageBroker restore", Cause: "PageBroker ARTIFACT_CORRUPT: bad index", SnapshotUnusable: true},
		},
		"a final code that names this node": {
			err:  untouched(pagebroker.NewFailureError(pagebroker.Failure_ACCESS_DENIED, "no access")),
			want: podcontract.FailureSummary{Step: "preparing PageBroker restore", Cause: "PageBroker ACCESS_DENIED: no access"},
		},
		"a container that stopped during the attempt": {
			err: stoppedDuringAttempt(untouched(storeUnreachable)),
			want: podcontract.FailureSummary{
				Step: "preparing PageBroker restore", Cause: "the container stopped during the attempt: PageBroker STORAGE_UNAVAILABLE: store unreachable",
			},
		},
		"a cause longer than the limit": {
			err:  errors.New(long),
			want: podcontract.FailureSummary{Cause: strings.Repeat("a", podcontract.FailureCauseMaxBytes-1)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := failureSummary(restoreFailureOf(tc.err))

			assert.Equal(t, tc.want, *got)
			assert.True(t, utf8.ValidString(got.Cause))
		})
	}
}

func TestRunRestoreRecordsTheLastFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		limit   string
		failure error
		wantErr error
		want    podcontract.FailureSummary
	}{
		"retried in place": {
			limit: "2", failure: untouched(storeUnreachable), wantErr: errRetryScheduled,
			want: podcontract.FailureSummary{Step: "preparing PageBroker restore", Cause: "PageBroker STORAGE_UNAVAILABLE: store unreachable"},
		},
		"ended by a changed container": {
			limit: "2", failure: changedByCRIU,
			want: podcontract.FailureSummary{Step: "restoring with CRIU", Cause: "criu restore failed"},
		},
		"ended by a final failure": {
			limit: "2", failure: untouched(pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_NOT_FOUND, "gone")),
			want: podcontract.FailureSummary{Step: "preparing PageBroker restore", Cause: "PageBroker ARTIFACT_NOT_FOUND: gone", SnapshotUnusable: true},
		},
		"ended without retries": {
			limit: "0", failure: untouched(storeUnreachable),
			want: podcontract.FailureSummary{Step: "preparing PageBroker restore", Cause: "PageBroker STORAGE_UNAVAILABLE: store unreachable"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit(tc.limit), nil)
			h.failAttempts(tc.failure)

			err := run()

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.Error(t, err)
			}
			record := h.volume.record(t, "host-uid", "main")
			require.NotNil(t, record.LastFailure)
			assert.Equal(t, tc.want, *record.LastFailure)
		})
	}
}

// The failure that ends a restore is saved after restore-failed. Saved
// first, a crash between the two would leave what looks like an unfinished
// attempt, and the next agent would retry a final failure.
func TestRunRestoreSavesTheEndingFailureAfterRestoreFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		limit   string
		record  func(*attemptHarness) restoreAttempts
		failure error
	}{
		"not retried": {limit: "2", failure: untouched(pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_CORRUPT, "bad index"))},
		"no retries left": {limit: "1", failure: untouched(storeUnreachable), record: func(h *attemptHarness) restoreAttempts {
			return restoreAttempts{Limit: 1, Attempt: 1, Deadline: h.now.Add(10 * time.Minute), ContainerID: "ctr-abc", RetryAt: h.now.Add(-time.Second)}
		}},
		"without retries": {limit: "0", failure: untouched(storeUnreachable)},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit(tc.limit), nil)
			if tc.record != nil {
				h.volume.put(t, "host-uid", "main", tc.record(h))
			}
			h.failAttempts(tc.failure)
			var order []string
			writeSentinel := h.w.writeControlSentinelFn
			h.w.writeControlSentinelFn = func(pid int, name string, data []byte) error {
				order = append(order, name)
				return writeSentinel(pid, name, data)
			}
			writeFile := h.w.writePodControlSentinelFn
			h.w.writePodControlSentinelFn = func(uid, container, name string, data []byte) error {
				if name == podcontract.RestoreAttemptsFile && strings.Contains(string(data), `"lastFailure"`) {
					order = append(order, "record with the failure")
				}
				return writeFile(uid, container, name, data)
			}

			require.Error(t, run())

			assert.Equal(t, []string{podcontract.RestoreFailedFile, "record with the failure"}, order)
		})
	}
}

// The next attempt and its success keep the failure before them, so the
// annotation still says why a retry was needed.
func TestRunRestoreKeepsTheLastFailureThroughTheNextAttempt(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	h.failAttempts(untouched(storeUnreachable))
	require.ErrorIs(t, run(), errRetryScheduled)

	h.now = h.volume.record(t, "host-uid", "main").RetryAt
	require.NoError(t, run())

	assert.Equal(t, 2, h.seen.Attempt)
	require.NotNil(t, h.seen.LastFailure)
	assert.Equal(t, "PageBroker STORAGE_UNAVAILABLE: store unreachable", h.seen.LastFailure.Cause)
	assert.Equal(t, h.seen.LastFailure, h.volume.record(t, "host-uid", "main").LastFailure)
}

func TestRestoreAttemptsHoldTheLongestCause(t *testing.T) {
	for name, cause := range map[string]string{
		"control characters": strings.Repeat("\x01", podcontract.FailureCauseMaxBytes),
		"HTML characters":    strings.Repeat("<", podcontract.FailureCauseMaxBytes),
		"invalid UTF-8":      strings.Repeat("\xff", podcontract.FailureCauseMaxBytes),
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := newAttemptHarness(t, retryLimit("2"), nil)
			op := &restoreOperation{controller: h.w, destination: "main", log: logr.Discard()}
			record := &restoreAttempts{
				Limit: 2, Attempt: 3, Deadline: h.now, RetryAt: h.now, ContainerID: strings.Repeat("c", 64),
				LastFailure: &podcontract.FailureSummary{Step: string(executor.StepValidateManifest), Cause: cause, SnapshotUnusable: true},
			}

			require.NoError(t, op.writeRestoreAttempts("host-uid", record))

			got := h.volume.record(t, "host-uid", "main")
			require.NotNil(t, got.LastFailure)
			assert.Equal(t, utf8.RuneCountInString(cause), utf8.RuneCountInString(got.LastFailure.Cause))
		})
	}
}

func TestWriteRestoreAttemptsRefusesARecordAPassCannotRead(t *testing.T) {
	h, _ := newAttemptHarness(t, retryLimit("2"), nil)
	op := &restoreOperation{controller: h.w, destination: "main", log: logr.Discard()}

	err := op.writeRestoreAttempts("host-uid", &restoreAttempts{Limit: 2, Attempt: 1, ContainerID: strings.Repeat("c", restoreAttemptsMaxBytes)})

	require.ErrorContains(t, err, "a pass reads")
	assert.Empty(t, h.volume.writes)
}

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

// Two destinations that fail in the same pass land in one write, after the
// workers return, and a pass that changes nothing writes nothing.
func TestRestorePodContainersPublishesTheRetryStatusOncePerPass(t *testing.T) {
	pod := multiRestorePod()
	pod.Annotations[podcontract.RestoreRetryLimitAnnotation] = "2"
	w := makeTestController(t, pod)
	w.config.Restore.TimeoutSeconds = 600
	w.runtime = &fakeRuntime{resolveContainerPID: 4242, podUID: "host-uid"}
	volume := newFakeControlVolume(w)
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		if req.DestinationContainerName == "engine-0" {
			return executor.RestoreResult{}, untouched(storeUnreachable)
		}
		return executor.RestoreResult{}, changedByCRIU
	}
	h := &retryStatusHarness{w: w, pod: pod, volume: volume}
	plan := &restorePlan{
		artifact: &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"},
		mappings: []podcontract.ContainerMapping{{Source: "main", Destination: "engine-0"}, {Source: "main", Destination: "engine-1"}},
	}

	assert.True(t, w.restorePodContainers(context.Background(), pod, plan, "inference/restore-worker"))

	applies := h.applies()
	require.Len(t, applies, 1)
	value, status := appliedRetryStatus(t, applies[0])
	assert.Equal(t, podcontract.RestoreRetryStatus{
		"engine-0": {Attempt: 1, RetryLimit: 2, LastFailure: podcontract.FailureSummary{Step: "preparing PageBroker restore", Cause: "PageBroker STORAGE_UNAVAILABLE: store unreachable"}},
		"engine-1": {Attempt: 1, RetryLimit: 2, LastFailure: podcontract.FailureSummary{Step: "restoring with CRIU", Cause: "criu restore failed"}},
	}, status)

	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	// The fake clientset lets the later status apply by the same field manager
	// drop the annotation; an API server tracks the two apart.
	live.Annotations[podcontract.RestoreRetryStatusAnnotation] = value
	assert.True(t, w.restorePodContainers(context.Background(), live, plan, "inference/restore-worker"))
	assert.Len(t, h.applies(), 1, "a pass that only waited wrote the retry status again")
}
