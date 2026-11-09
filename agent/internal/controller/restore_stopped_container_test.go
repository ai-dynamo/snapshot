// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestRunRestoreEndsAnAttemptWhoseContainerStopped(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	restore := h.w.restoreFn
	h.w.restoreFn = func(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req executor.RestoreRequest, mounter executor.RestoreMounter) (executor.RestoreResult, error) {
		_, _ = restore(ctx, rt, log, req, mounter)
		h.w.RestoreContainerStopped(h.pod.UID, "main", "ctr-abc")
		select {
		case <-ctx.Done():
			return executor.RestoreResult{}, untouched(ctx.Err())
		case <-time.After(5 * time.Second):
			return executor.RestoreResult{}, untouched(errors.New("the attempt was not ended"))
		}
	}

	err := run()

	require.ErrorIs(t, err, errContainerStopped)
	record := h.volume.record(t, "host-uid", "main")
	assert.True(t, record.RetryAt.IsZero(), "a stopped container was retried in place")
	failed := h.events(restoreAttemptFailedReason)
	require.Len(t, failed, 1)
	assert.Equal(t, "Attempt 1 of 3 for destination main failed: while preparing PageBroker restore: "+
		"the container stopped during the attempt: context canceled; not retried: another attempt needs a new container", failed[0])
	assert.Zero(t, h.runtime().runningCalls, "the reported stop is enough to know the container changed")
	assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
	_, running := h.w.runningAttempts.Load("ctr-abc")
	assert.False(t, running, "the ended attempt is still registered")
}

// A container can stop before the hook reports it, or between the attempt's
// last touch of it and its failure. The agent asks the runtime once the
// attempt fails.
func TestRunRestoreEndsWhenTheContainerStoppedBeforeTheFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		annotations map[string]string
		exited      bool
		runningErr  error
		wantEnded   bool
		wantChecks  int
	}{
		"a container that stopped":       {annotations: retryLimit("2"), exited: true, wantEnded: true, wantChecks: 1},
		"a container that runs":          {annotations: retryLimit("2"), wantChecks: 1},
		"a state the runtime cannot say": {annotations: retryLimit("2"), runningErr: errors.New("CRI unavailable"), wantChecks: 1},
		"a limit of 0, as before": {
			annotations: map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}, exited: true, wantEnded: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, tc.annotations, nil)
			if tc.exited {
				h.runtime().exit("ctr-abc")
			}
			h.runtime().runningErr = tc.runningErr
			h.failAttempts(untouched(storeUnreachable))

			err := run()

			assert.Equal(t, tc.wantChecks, h.runtime().runningCalls)
			if !tc.wantEnded {
				require.ErrorIs(t, err, errRetryScheduled)
				return
			}
			require.ErrorIs(t, err, storeUnreachable)
			assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
			if tc.wantChecks > 0 {
				require.ErrorIs(t, err, errContainerStopped)
				failed := h.events(restoreAttemptFailedReason)
				require.Len(t, failed, 1)
				assert.Contains(t, failed[0], "the container stopped during the attempt: PageBroker STORAGE_UNAVAILABLE")
				assert.Contains(t, failed[0], "; not retried: another attempt needs a new container")
			}
		})
	}
}

func (h *attemptHarness) runtime() *fakeRuntime { return h.w.runtime.(*fakeRuntime) }

func TestRunRestoreEndsOnlyTheAttemptInTheStoppedContainer(t *testing.T) {
	for name, tc := range map[string]struct {
		annotations map[string]string
		stopped     string
		wantEnded   bool
	}{
		"its container":           {annotations: retryLimit("2"), stopped: "ctr-abc", wantEnded: true},
		"another container":       {annotations: retryLimit("2"), stopped: "ctr-other"},
		"a limit of 0, as before": {annotations: map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}, stopped: "ctr-abc"},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, tc.annotations, nil)
			var ended bool
			h.w.restoreFn = func(ctx context.Context, _ snapshotruntime.Runtime, _ logr.Logger, _ executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
				h.w.RestoreContainerStopped(h.pod.UID, "main", tc.stopped)
				ended = ctx.Err() != nil
				return executor.RestoreResult{}, untouched(storeUnreachable)
			}

			err := run()

			assert.Equal(t, tc.wantEnded, ended)
			if !tc.wantEnded {
				assert.NotContains(t, err.Error()+" "+strings.Join(h.events(restoreAttemptFailedReason), " "), errContainerStopped.Error())
			}
			_, running := h.w.runningAttempts.Load("ctr-abc")
			assert.False(t, running)
		})
	}
}

func TestRunRestoreDoesNotTakeTheDeadlineForAStop(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	h.volume.put(t, "host-uid", "main", restoreAttempts{Limit: 2, Attempt: 1, Deadline: h.now.Add(50 * time.Millisecond), ContainerID: "ctr-abc"})
	h.w.restoreFn = func(ctx context.Context, _ snapshotruntime.Runtime, _ logr.Logger, _ executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		<-ctx.Done()
		return executor.RestoreResult{}, untouched(ctx.Err())
	}

	require.ErrorIs(t, run(), context.DeadlineExceeded)

	failed := h.events(restoreAttemptFailedReason)
	require.Len(t, failed, 1)
	assert.Equal(t, "Attempt 1 of 3 for destination main failed: while preparing PageBroker restore: context deadline exceeded; not retried: the restore timed out", failed[0])
}

func TestRunRestoreForgetsTheAttemptWhenItEnds(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)

	require.NoError(t, run())

	_, running := h.w.runningAttempts.Load("ctr-abc")
	assert.False(t, running)
	h.w.RestoreContainerStopped(h.pod.UID, "main", "ctr-abc")
}

func TestStoppedDuringAttempt(t *testing.T) {
	notFound := pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_NOT_FOUND, "no such artifact")
	original := untouched(notFound)

	got := restoreFailureOf(stoppedDuringAttempt(original))

	assert.Equal(t, executor.StepPrepareRestore, got.Step)
	assert.True(t, got.ContainerChanged)
	assert.ErrorIs(t, got, errContainerStopped)
	assert.Equal(t, pagebroker.Failure_ARTIFACT_NOT_FOUND, failureCode(got.Cause), "the stop hid a final code")
	assert.Equal(t, endRestore, chooseRecovery(got, nil, budget{attempts: 2, time: time.Minute}))
	assert.False(t, original.ContainerChanged, "the attempt's own failure was changed")
	assert.Same(t, notFound, original.Cause)
}
