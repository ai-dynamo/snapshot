// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// changedByCRIU is a failure after nsrestore started changing the container.
var changedByCRIU = &executor.RestoreFailure{Step: executor.StepRestoreCRIU, Cause: errors.New("criu restore failed"), ContainerChanged: true}

// interrupted is the record an agent leaves when it stops during attempt 1.
func (h *attemptHarness) interrupted(containerID string) restoreAttempts {
	return restoreAttempts{Limit: 2, Attempt: 1, Deadline: h.now.Add(time.Minute), ContainerID: containerID}
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
		record      func(*attemptHarness) restoreAttempts
		wantAttempt int
	}{
		"with retries": {
			record:      func(h *attemptHarness) restoreAttempts { return h.interrupted("ctr-abc") },
			wantAttempt: 1,
		},
		"a retry in place that had started": {
			record: func(h *attemptHarness) restoreAttempts {
				return restoreAttempts{Limit: 2, Attempt: 2, Deadline: h.now.Add(time.Minute), ContainerID: "ctr-abc"}
			},
			wantAttempt: 2,
		},
		"the last attempt": {
			record: func(h *attemptHarness) restoreAttempts {
				return restoreAttempts{Limit: 1, Attempt: 2, Deadline: h.now.Add(time.Minute), ContainerID: "ctr-abc"}
			},
			wantAttempt: 2,
		},
		"in a container kubelet started since": {
			record:      func(h *attemptHarness) restoreAttempts { return h.interrupted("ctr-old") },
			wantAttempt: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("2"), nil)
			h.recovering = true
			record := tc.record(h)
			h.volume.put(t, "host-uid", "main", record)

			require.NoError(t, run())

			assert.Equal(t, 1, h.restores)
			assert.Equal(t, tc.wantAttempt, h.seen.Attempt)
			assert.Equal(t, record.Limit, h.seen.Limit)
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
	h.volume.put(t, "host-uid", "main", restoreAttempts{Limit: 2, Attempt: 1, Deadline: h.now.Add(-time.Second), ContainerID: "ctr-abc"})

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
	h.volume.put(t, "host-uid", "main", h.interrupted("ctr-abc"))

	require.NoError(t, run())

	assert.Equal(t, 0, h.restores)
	assert.Empty(t, h.calls)
	assert.Empty(t, h.volume.writes, "the pass changed the attempt record")
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
			record := h.volume.record(t, "host-uid", "main")
			assert.Equal(t, 1, record.Attempt)
			assert.True(t, record.RetryAt.IsZero(), "a stopping agent scheduled a retry")
			assert.Empty(t, h.events(restoreAttemptFailedReason))
			_, running := h.w.runningAttempts.Load("ctr-abc")
			assert.False(t, running)

			h.recovering = true
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

			h.recovering = true
			require.NoError(t, run())

			assert.Equal(t, []string{"write " + podcontract.RestoreCompleteFile}, h.calls)
			assert.Equal(t, 2, h.restores)
			assert.Empty(t, h.volume.reads)
			assert.Empty(t, h.volume.writes)
		})
	}
}
