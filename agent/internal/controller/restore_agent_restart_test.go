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

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// changedByCRIU is a failure after nsrestore started changing the container.
var changedByCRIU = &executor.RestoreFailure{Step: executor.StepRestoreCRIU, Cause: errors.New("criu restore failed"), ContainerChanged: true}

// interrupted is the record an agent leaves when it stops during attempt 1.
func (h *attemptHarness) interrupted(containerID string) podcontract.ContainerRetryStatus {
	return podcontract.ContainerRetryStatus{RetryLimit: 2, Attempt: 1, Deadline: h.now.Add(time.Minute), ContainerID: containerID}
}

// leaveAttempts sets the retry status an agent left for the destination
// before it stopped.
func (h *attemptHarness) leaveAttempts(t *testing.T, record podcontract.ContainerRetryStatus) {
	t.Helper()
	record.PodUID = h.pod.UID
	leaveRetryStatus(t, h.pod, podcontract.RestoreRetryStatus{"main": record})
}

// restartAgent stands in for an agent restart: the Pod keeps the retry status
// the old agent wrote, and the new agent remembers no attempt.
func (h *attemptHarness) restartAgent(t *testing.T) {
	t.Helper()
	writeQueuedRetryStatus(h.w)
	live, err := h.w.clientset.CoreV1().Pods(h.pod.Namespace).Get(context.Background(), h.pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	h.pod.Annotations = live.Annotations
	for _, memory := range []*sync.Map{&h.w.restoreAttemptRecords, &h.w.attemptSeeds, &h.w.retryStatusWritten} {
		memory.Clear()
	}
	h.recovering = true
}

// runStopping runs a pass whose agent starts stopping during the attempt,
// which then fails with failure.
func (h *attemptHarness) runStopping(failure error) error {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	restore := h.w.restoreFn
	defer func() { h.w.restoreFn = restore }()
	h.w.restoreFn = func(attemptCtx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req executor.RestoreRequest, mounter executor.RestoreMounter) (executor.RestoreResult, error) {
		_, _ = restore(attemptCtx, rt, log, req, mounter)
		stop()
		return executor.RestoreResult{}, failure
	}
	artifact := &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"}
	return h.w.runRestore(ctx, h.pod, &restorePlan{artifact: artifact}, "main", h.containerID, h.now, h.recovering)
}

// An attempt that no agent saw finish runs again, as it always did without
// retries: within its deadline, under its number, and in the container the
// destination runs now.
func TestRunRestoreRepeatsAnInterruptedAttempt(t *testing.T) {
	for name, tc := range map[string]struct {
		record      func(*attemptHarness) podcontract.ContainerRetryStatus
		wantAttempt int
	}{
		"with retries": {
			record:      func(h *attemptHarness) podcontract.ContainerRetryStatus { return h.interrupted("ctr-abc") },
			wantAttempt: 1,
		},
		"a retry in place that had started": {
			record: func(h *attemptHarness) podcontract.ContainerRetryStatus {
				return podcontract.ContainerRetryStatus{RetryLimit: 2, Attempt: 2, Deadline: h.now.Add(time.Minute), ContainerID: "ctr-abc"}
			},
			wantAttempt: 2,
		},
		"the last attempt": {
			record: func(h *attemptHarness) podcontract.ContainerRetryStatus {
				return podcontract.ContainerRetryStatus{RetryLimit: 1, Attempt: 2, Deadline: h.now.Add(time.Minute), ContainerID: "ctr-abc"}
			},
			wantAttempt: 2,
		},
		"in a container kubelet started since": {
			record:      func(h *attemptHarness) podcontract.ContainerRetryStatus { return h.interrupted("ctr-old") },
			wantAttempt: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("2"), nil)
			h.recovering = true
			record := tc.record(h)
			h.leaveAttempts(t, record)

			require.NoError(t, run())

			assert.Equal(t, 1, h.restores)
			assert.Equal(t, tc.wantAttempt, h.seen.Attempt)
			assert.Equal(t, record.RetryLimit, h.seen.RetryLimit)
			assert.Equal(t, "ctr-abc", h.seen.ContainerID)
			assert.True(t, record.Deadline.Equal(h.deadline), "the repeat got %s, not the attempt's deadline", h.deadline)
			assert.Equal(t, []string{"write " + podcontract.RestoreCompleteFile}, h.calls)
			assert.Empty(t, h.events(restoreAttemptFailedReason), "the interrupted attempt was reported as failed")
			assert.Empty(t, h.events(restoreRetriesExhaustedReason))
		})
	}
}

func TestRunRestoreEndsAnInterruptedAttemptPastItsDeadline(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	h.recovering = true
	h.leaveAttempts(t, podcontract.ContainerRetryStatus{RetryLimit: 2, Attempt: 1, Deadline: h.now.Add(-time.Second), ContainerID: "ctr-abc"})

	err := run()

	require.ErrorContains(t, err, "before attempt 1 could start")
	assert.Equal(t, 0, h.restores)
	assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
	assert.Empty(t, h.events(restoreAttemptFailedReason))
	assert.Len(t, h.events(restoreRetriesExhaustedReason), 1)
}

// A completed restore runs only its post-restore action, so CRIU never runs
// again and a record that looks interrupted is not repeated.
func TestRunRestoreDoesNotRepeatACompletedRestore(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	h.recovering = true
	h.pod.Status.Conditions = append(h.pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodConditionType(podcontract.RestoredCondition),
		Status: corev1.ConditionFalse,
		Reason: podcontract.RestoreReasonInProgress,
	})
	h.w.controlSentinelExistsFn = func(_ int, name string) (bool, error) { return name == podcontract.RestoreCompleteFile, nil }
	h.leaveAttempts(t, h.interrupted("ctr-abc"))

	require.NoError(t, run())

	assert.Equal(t, 0, h.restores)
	assert.Empty(t, h.calls)
	assert.Zero(t, h.w.retryStatusQueue.Len(), "the pass changed the attempt record")
	assert.Empty(t, h.events(restoreAttemptFailedReason))
}

// A stopping agent records nothing and kills nothing, so the next agent finds
// an attempt that never finished.
func TestRunRestoreLeavesAnAttemptToTheNextAgent(t *testing.T) {
	for name, failure := range map[string]error{
		"untouched": untouched(context.Canceled),
		"changed":   changedByCRIU,
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("2"), nil)

			require.ErrorIs(t, h.runStopping(failure), errRestoreInterrupted)

			assert.Equal(t, 1, h.restores)
			assert.Empty(t, h.calls, "a stopping agent wrote restore-failed or killed the container")
			assert.Empty(t, h.events(restoreAttemptFailedReason))
			_, running := h.w.runningAttempts.Load("ctr-abc")
			assert.False(t, running)

			h.restartAgent(t)
			status, found := publishedRetryStatus(t, h.w, h.pod)
			require.True(t, found)
			assert.Equal(t, 1, status["main"].Attempt)
			assert.True(t, status["main"].RetryAt.IsZero(), "a stopping agent scheduled a retry")
			require.NoError(t, run())

			assert.Equal(t, []string{"write " + podcontract.RestoreCompleteFile}, h.calls)
			assert.Equal(t, 2, h.restores)
			assert.Equal(t, 1, h.seen.Attempt, "the next agent counted the interrupted attempt")
		})
	}
}

// Without retries a stopping agent leaves no record, so the next agent starts
// the restore over, as before retries.
func TestRunRestoreWithoutRetriesLeavesTheRestoreToTheNextAgent(t *testing.T) {
	for name, failure := range map[string]error{
		"untouched": untouched(context.Canceled),
		"changed":   changedByCRIU,
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("0"), nil)

			require.ErrorIs(t, h.runStopping(failure), errRestoreInterrupted)

			assert.Equal(t, 1, h.restores)
			assert.Empty(t, h.calls, "a stopping agent wrote restore-failed or killed the container")

			h.restartAgent(t)
			require.NoError(t, run())

			assert.Equal(t, []string{"write " + podcontract.RestoreCompleteFile}, h.calls)
			assert.Equal(t, 2, h.restores)
			assert.Nil(t, savedAttempts(h.w, h.pod.UID, "main"))
			assert.Empty(t, retryStatusApplies(h.w))
		})
	}
}

// An agent that restarts while a retry waits starts it when it falls due, as
// the next attempt, within the first attempt's deadline.
func TestRunRestorePicksUpAWaitingRetryAfterARestart(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	h.w.config.Restore.TimeoutSeconds = 600
	h.failAttempts(untouched(storeUnreachable))
	started := h.now
	require.ErrorIs(t, run(), errRetryScheduled)

	h.restartAgent(t)
	h.now = started.Add(29 * time.Second)
	require.ErrorIs(t, run(), errRetryScheduled)
	assert.Equal(t, 1, h.restores, "a retry started before it was due")

	h.now = started.Add(30 * time.Second)
	require.NoError(t, run())

	assert.Equal(t, 2, h.restores)
	assert.Equal(t, 2, h.seen.Attempt)
	assert.True(t, started.Add(10*time.Minute).Equal(h.deadline), "the retry got %s, not the first attempt's deadline", h.deadline)
	assert.Equal(t, []string{"write " + podcontract.RestoreCompleteFile}, h.calls)
}
