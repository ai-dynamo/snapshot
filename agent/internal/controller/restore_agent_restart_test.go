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

// An attempt that no agent saw finish runs again at every retry limit, as it
// always did without retries: within its deadline, under its number, and in
// the container the destination runs now.
func TestRunRestoreRepeatsAnInterruptedAttemptAtEveryLimit(t *testing.T) {
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
		"without retries": {
			record: func(h *attemptHarness) restoreAttempts {
				return restoreAttempts{Attempt: 1, Deadline: h.now.Add(time.Minute), ContainerID: "ctr-abc"}
			},
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

// An outcome already written ends the pass before the record is read, so
// CRIU never runs again and an interrupted-looking record is not retried.
func TestRunRestoreReportsAWrittenOutcomeAgain(t *testing.T) {
	for name, tc := range map[string]struct {
		sentinels []string
		wantErr   error
	}{
		"restore-failed":            {sentinels: []string{podcontract.RestoreFailedFile}, wantErr: errRestoreAlreadyFailed},
		"restore-complete":          {sentinels: []string{podcontract.RestoreCompleteFile}},
		"both, after a failed post": {sentinels: []string{podcontract.RestoreCompleteFile, podcontract.RestoreFailedFile}, wantErr: errRestoreAlreadyFailed},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("2"), nil)
			h.recovering = true
			h.volume.put(t, "host-uid", "main", h.interrupted("ctr-abc"))
			for _, name := range tc.sentinels {
				h.volume.putSentinel("host-uid", "main", name)
			}

			err := run()

			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
			}
			assert.Equal(t, 0, h.restores)
			assert.Empty(t, h.calls)
			assert.Empty(t, h.volume.writes, "the pass changed the attempt record")
			assert.Empty(t, h.events(restoreAttemptFailedReason))
		})
	}
}

// A destination that failed before an agent restart reports the failure its
// record kept, not only that it failed.
func TestRunRestoreReportsTheFailureThatEndedAnEarlierPass(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	h.recovering = true
	record := h.interrupted("ctr-abc")
	record.LastFailure = &podcontract.FailureSummary{Step: string(executor.StepRestoreCRIU), Cause: "criu exited with status 1"}
	h.volume.put(t, "host-uid", "main", record)
	h.volume.putSentinel("host-uid", "main", podcontract.RestoreFailedFile)

	err := run()

	require.ErrorIs(t, err, errRestoreAlreadyFailed)
	assert.Equal(t, "the restore already failed in an earlier pass: while restoring with CRIU: criu exited with status 1", err.Error())
	assert.Equal(t, executor.StepRestoreCRIU, executor.StepOf(err))
	assert.Equal(t, 0, h.restores)
	assert.Empty(t, h.calls)
	assert.Empty(t, h.volume.writes, "the pass changed the attempt record")
}

// Only a sentinel of this destination, under the Pod's UID on the node, and
// on a pass that finds the restore in progress counts as an outcome.
func TestRunRestoreIgnoresOtherSentinels(t *testing.T) {
	for name, tc := range map[string]struct {
		uid, container string
		recovering     bool
	}{
		"under the Pod's own UID": {uid: "restore-pod-uid", container: "main", recovering: true},
		"of another container":    {uid: "host-uid", container: "other", recovering: true},
		"on the first pass":       {uid: "host-uid", container: "main"},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("2"), nil)
			h.recovering = tc.recovering
			h.volume.putSentinel(tc.uid, tc.container, podcontract.RestoreFailedFile)

			require.NoError(t, run())

			assert.Equal(t, 1, h.restores)
			assert.Equal(t, 1, h.seen.Attempt)
		})
	}
}

func TestRunRestoreEndsThePassWhenItCannotCheckTheOutcome(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	h.recovering = true
	h.w.podControlSentinelExistsFn = func(_, _, name string) (bool, error) {
		return false, errors.New("control sentinel restore-failed is not a regular file")
	}

	err := run()

	require.ErrorContains(t, err, "check for "+podcontract.RestoreFailedFile+": control sentinel restore-failed is not a regular file")
	assert.Equal(t, 0, h.restores)
	assert.Empty(t, h.calls)
}

// A stopping agent records nothing and kills nothing, so the next agent finds
// an attempt that never finished.
func TestRunRestoreLeavesAnAttemptToTheNextAgent(t *testing.T) {
	for name, tc := range map[string]struct {
		limit   string
		failure error
	}{
		"without retries, untouched": {limit: "0", failure: untouched(context.Canceled)},
		"without retries, changed":   {limit: "0", failure: changedByCRIU},
		"with retries, untouched":    {limit: "2", failure: untouched(context.Canceled)},
		"with retries, changed":      {limit: "2", failure: changedByCRIU},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit(tc.limit), nil)

			require.ErrorIs(t, h.runStopping(tc.failure), errRestoreInterrupted)

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
