// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"math"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

var storeUnreachable = pagebroker.NewFailureError(pagebroker.Failure_STORAGE_UNAVAILABLE, "store unreachable")

// untouched is a failure that left the container as kubelet started it.
func untouched(cause error) *executor.RestoreFailure {
	return &executor.RestoreFailure{Step: executor.StepPrepareRestore, Cause: cause}
}

func TestChooseRecovery(t *testing.T) {
	left := budget{attempts: 1, time: time.Minute}
	changed := &executor.RestoreFailure{Step: executor.StepRestoreCRIU, Cause: errors.New("criu restore failed"), ContainerChanged: true}
	cases := map[string]struct {
		failure *executor.RestoreFailure
		ctxErr  error
		left    budget
		want    recovery
	}{
		"an untouched container with retries left": {failure: untouched(storeUnreachable), left: left, want: retryInPlace},
		"a failure without a PageBroker code":      {failure: untouched(errors.New("dial unix: no such file")), left: left, want: retryInPlace},
		"TRANSACTION_NOT_FOUND, a lost handle": {
			failure: untouched(pagebroker.NewFailureError(pagebroker.Failure_TRANSACTION_NOT_FOUND, "expired")), left: left, want: retryInPlace,
		},
		"a final failure": {
			failure: &executor.RestoreFailure{Step: executor.StepValidateManifest, Cause: errors.New("mismatch"), Final: true}, left: left, want: endRestore,
		},
		"a final failure on the last attempt": {
			failure: &executor.RestoreFailure{Step: executor.StepValidateManifest, Cause: errors.New("mismatch"), Final: true}, want: endRestore,
		},
		"the restore timed out": {failure: untouched(storeUnreachable), ctxErr: context.DeadlineExceeded, left: left, want: endRestore},
		"the agent is stopping": {failure: untouched(storeUnreachable), ctxErr: context.Canceled, left: left, want: endRestore},
		"no attempts left":      {failure: untouched(storeUnreachable), left: budget{time: time.Minute}, want: retriesExhausted},
		"no time left":          {failure: untouched(storeUnreachable), left: budget{attempts: 1}, want: retriesExhausted},
		"time already past":     {failure: untouched(storeUnreachable), left: budget{attempts: 1, time: -time.Second}, want: retriesExhausted},
		"a changed container":   {failure: changed, left: left, want: endRestore},
		"a changed container, no attempts left": {
			failure: changed, left: budget{time: time.Minute}, want: endRestore,
		},
	}
	for code := range finalCodes {
		cases[code.String()+" in a step error"] = struct {
			failure *executor.RestoreFailure
			ctxErr  error
			left    budget
			want    recovery
		}{
			failure: untouched(&executor.StepError{Step: executor.StepPrepareRestore, Err: pagebroker.NewFailureError(code, "refused")}),
			left:    left,
			want:    endRestore,
		}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, chooseRecovery(tc.failure, tc.ctxErr, tc.left))
		})
	}
}

func TestFinalCodesNameTheArtifactOrTheRequest(t *testing.T) {
	var final []string
	for code := range finalCodes {
		final = append(final, code.String())
	}
	assert.ElementsMatch(t, []string{
		"ARTIFACT_NOT_FOUND", "ARTIFACT_CORRUPT", "UNSUPPORTED_ARTIFACT", "ACCESS_DENIED", "STORE_MISMATCH", "INVALID_REQUEST",
	}, final)
}

func TestRestoreRetryBackoff(t *testing.T) {
	for attempt, want := range map[int]time.Duration{
		0:    30 * time.Second,
		1:    30 * time.Second,
		2:    time.Minute,
		3:    2 * time.Minute,
		4:    4 * time.Minute,
		5:    5 * time.Minute,
		6:    5 * time.Minute,
		1000: 5 * time.Minute,
	} {
		assert.Equal(t, want, restoreRetryBackoff(attempt), "attempt %d", attempt)
	}
}

func TestBudgetLeft(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		record restoreAttempts
		want   budget
	}{
		"less the wait": {
			record: restoreAttempts{Limit: 2, Attempt: 2, Deadline: now.Add(10 * time.Minute)},
			want:   budget{attempts: 1, time: 9 * time.Minute},
		},
		"the last attempt": {
			record: restoreAttempts{Limit: 1, Attempt: 2, Deadline: now.Add(10 * time.Minute)},
			want:   budget{time: 9 * time.Minute},
		},
		"no deadline": {
			record: restoreAttempts{Limit: 1, Attempt: 1},
			want:   budget{attempts: 1, time: time.Duration(math.MaxInt64)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, budgetLeft(&tc.record, now))
		})
	}
}

func TestRestoreFailureOf(t *testing.T) {
	failure := untouched(storeUnreachable)
	assert.Same(t, failure, restoreFailureOf(failure))

	stepped := restoreFailureOf(&executor.StepError{Step: executor.StepRunNSRestore, Err: errors.New("exit status 1")})
	assert.Equal(t, executor.StepRunNSRestore, stepped.Step)
	assert.Equal(t, "while running nsrestore: exit status 1", stepped.Error(), "the step is named once")
	assert.False(t, stepped.Final)
	assert.False(t, stepped.ContainerChanged)

	plain := restoreFailureOf(errors.New("boom"))
	assert.Empty(t, plain.Step)
	assert.Equal(t, "boom", plain.Error())
}

// failAttempts makes the harness's attempts fail with errs in turn, then
// succeed.
func (h *attemptHarness) failAttempts(errs ...error) {
	restore := h.w.restoreFn
	h.w.restoreFn = func(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req executor.RestoreRequest, mounter executor.RestoreMounter) (executor.RestoreResult, error) {
		result, _ := restore(ctx, rt, log, req, mounter)
		if len(errs) == 0 {
			return result, nil
		}
		err := errs[0]
		errs = errs[1:]
		return executor.RestoreResult{}, err
	}
}

func (h *attemptHarness) events(reason string) []string {
	var messages []string
	for _, event := range eventsForReason(h.w.clientset.(*fake.Clientset), reason) {
		messages = append(messages, event.Message)
	}
	return messages
}

func retryLimit(limit string) map[string]string {
	return map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a", podcontract.RestoreRetryLimitAnnotation: limit}
}

func TestRunRestoreRetriesInPlaceOnceEachRetryIsDue(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	h.w.config.Restore.TimeoutSeconds = 600
	h.failAttempts(untouched(storeUnreachable), untouched(storeUnreachable))
	started := h.now

	require.ErrorIs(t, run(), errRetryScheduled)
	record := h.volume.record(t, "host-uid", "main")
	assert.Equal(t, 1, record.Attempt)
	assert.True(t, started.Add(30*time.Second).Equal(record.RetryAt), "retry at %s", record.RetryAt)
	assert.Empty(t, h.calls, "an attempt retried in place wrote restore-failed or killed the container")

	h.now = started.Add(29 * time.Second)
	require.ErrorIs(t, run(), errRetryScheduled)
	assert.Equal(t, 1, h.restores, "a retry started before it was due")

	h.now = started.Add(30 * time.Second)
	require.ErrorIs(t, run(), errRetryScheduled)
	assert.Equal(t, 2, h.restores)
	assert.Equal(t, 2, h.seen.Attempt)
	assert.True(t, h.seen.RetryAt.IsZero(), "the attempt that started still shows a retry due")
	record = h.volume.record(t, "host-uid", "main")
	assert.True(t, started.Add(90*time.Second).Equal(record.RetryAt), "the second wait is twice the first: retry at %s", record.RetryAt)

	h.now = started.Add(90 * time.Second)
	require.NoError(t, run())
	assert.Equal(t, 3, h.restores)
	assert.Equal(t, 3, h.seen.Attempt)
	assert.True(t, started.Add(10*time.Minute).Equal(h.deadline), "the retry got %s, not the first attempt's deadline", h.deadline)
	assert.Equal(t, []string{"write " + podcontract.RestoreCompleteFile}, h.calls)

	requested := h.events(restoreRequestedReason)
	require.Len(t, requested, 3)
	assert.Contains(t, requested[0], "for destination main (attempt 1 of 3)")
	assert.Contains(t, requested[2], "for destination main (attempt 3 of 3)")
	failed := h.events(restoreAttemptFailedReason)
	require.Len(t, failed, 2)
	assert.Equal(t, "Attempt 1 of 3 for destination main failed: while preparing PageBroker restore: "+
		"PageBroker STORAGE_UNAVAILABLE: store unreachable; retrying in place in 30s", failed[0])
	assert.Contains(t, failed[1], "Attempt 2 of 3 for destination main failed")
	assert.Contains(t, failed[1], "retrying in place in 1m0s")
	assert.Empty(t, h.events(restoreRetriesExhaustedReason))
}

func TestRunRestoreEndsWithoutRetryingWhatNoAttemptChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		failure *executor.RestoreFailure
		want    string
	}{
		"a final failure": {
			failure: &executor.RestoreFailure{Step: executor.StepValidateManifest, Cause: errors.New("checkpoint is for another image"), Final: true},
			want:    "not retried: no other attempt can succeed",
		},
		"a final PageBroker code": {
			failure: untouched(pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_CORRUPT, "bad index")),
			want:    "not retried: PageBroker reported ARTIFACT_CORRUPT",
		},
		"a changed container": {
			failure: &executor.RestoreFailure{Step: executor.StepRestoreCRIU, Cause: errors.New("criu restore failed"), ContainerChanged: true},
			want:    "not retried: another attempt needs a new container",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("2"), nil)
			h.failAttempts(tc.failure)

			err := run()

			var failure *executor.RestoreFailure
			require.ErrorAs(t, err, &failure)
			assert.Same(t, tc.failure, failure)
			assert.Equal(t, 1, h.restores)
			assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
			failed := h.events(restoreAttemptFailedReason)
			require.Len(t, failed, 1)
			assert.Contains(t, failed[0], "Attempt 1 of 3 for destination main failed")
			assert.Contains(t, failed[0], tc.want)
			assert.Empty(t, h.events(restoreRetriesExhaustedReason))
		})
	}
}

func TestRunRestoreEndsWhenTheAttemptTimedOut(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	h.volume.put(t, "host-uid", "main", restoreAttempts{Limit: 2, Attempt: 1, Deadline: h.now.Add(50 * time.Millisecond), ContainerID: "ctr-abc"})
	restore := h.w.restoreFn
	h.w.restoreFn = func(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req executor.RestoreRequest, mounter executor.RestoreMounter) (executor.RestoreResult, error) {
		_, _ = restore(ctx, rt, log, req, mounter)
		<-ctx.Done()
		return executor.RestoreResult{}, untouched(ctx.Err())
	}

	err := run()

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
	failed := h.events(restoreAttemptFailedReason)
	require.Len(t, failed, 1)
	assert.Contains(t, failed[0], "not retried: the restore timed out")
	assert.Empty(t, h.events(restoreRetriesExhaustedReason))
}

func TestRunRestoreEndsWhenRetriesRunOut(t *testing.T) {
	for name, tc := range map[string]struct {
		limit         string
		wantErr       string
		wantExhausted string
	}{
		"out of attempts": {
			limit:         "1",
			wantErr:       "restore used all 2 attempts: while preparing PageBroker restore: PageBroker STORAGE_UNAVAILABLE",
			wantExhausted: "Destination main: restore used all 2 attempts",
		},
		"out of time for the next wait": {
			limit:         "3",
			wantErr:       "restore ran out of time after attempt 2 of 4: while preparing PageBroker restore",
			wantExhausted: "Destination main: restore ran out of time after attempt 2 of 4",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit(tc.limit), nil)
			h.failAttempts(untouched(storeUnreachable), untouched(storeUnreachable))
			require.ErrorIs(t, run(), errRetryScheduled)
			h.now = h.now.Add(30 * time.Second)

			err := run()

			require.ErrorContains(t, err, tc.wantErr)
			require.ErrorIs(t, err, storeUnreachable)
			assert.Equal(t, 2, h.restores)
			assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
			failed := h.events(restoreAttemptFailedReason)
			require.Len(t, failed, 2)
			assert.Contains(t, failed[1], "; no retries left")
			exhausted := h.events(restoreRetriesExhaustedReason)
			require.Len(t, exhausted, 1)
			assert.Contains(t, exhausted[0], tc.wantExhausted)
		})
	}
}

// With a limit of 0 a restore fails as it did before retries: the same error
// and no event the retries added.
func TestRunRestoreWithoutRetriesFailsAsBefore(t *testing.T) {
	h, run := newAttemptHarness(t, map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}, nil)
	failure := untouched(storeUnreachable)
	h.failAttempts(failure)

	err := run()

	assert.Same(t, failure, err)
	assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
	assert.Equal(t, []string{"Restore requested from PodSnapshot snapshot-a for destination main"}, h.events(restoreRequestedReason))
	assert.Empty(t, h.events(restoreAttemptFailedReason))
	assert.Empty(t, h.events(restoreRetriesExhaustedReason))
}

func TestRunRestoreEndsWhenADueRetryHasNothingLeft(t *testing.T) {
	for name, tc := range map[string]struct {
		record  func(now time.Time) restoreAttempts
		wantErr string
	}{
		"past the deadline": {
			record: func(now time.Time) restoreAttempts {
				return restoreAttempts{Limit: 2, Attempt: 1, Deadline: now.Add(-time.Second), RetryAt: now.Add(-5 * time.Second), ContainerID: "ctr-abc"}
			},
			wantErr: "restore ran out of time after attempt 1 of 3: restore timed out at",
		},
		"over the limit": {
			record: func(now time.Time) restoreAttempts {
				return restoreAttempts{Limit: 1, Attempt: 2, Deadline: now.Add(time.Minute), RetryAt: now.Add(-time.Second), ContainerID: "ctr-abc"}
			},
			wantErr: "restore used all 2 attempts: attempt 3 is over the limit of 2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("2"), nil)
			h.volume.put(t, "host-uid", "main", tc.record(h.now))

			err := run()

			require.ErrorContains(t, err, tc.wantErr)
			assert.Equal(t, 0, h.restores)
			assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
			assert.Len(t, h.events(restoreRetriesExhaustedReason), 1)
			assert.Empty(t, h.events(restoreRequestedReason))
		})
	}
}

func TestRunRestoreEndsWhenTheRetryCannotBeSaved(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	failure := untouched(storeUnreachable)
	restore := h.w.restoreFn
	h.w.restoreFn = func(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req executor.RestoreRequest, mounter executor.RestoreMounter) (executor.RestoreResult, error) {
		_, _ = restore(ctx, rt, log, req, mounter)
		h.volume.mu.Lock()
		h.volume.writeErrs[podcontract.RestoreAttemptsFile] = syscall.ENOSPC
		h.volume.mu.Unlock()
		return executor.RestoreResult{}, failure
	}

	err := run()

	require.ErrorIs(t, err, syscall.ENOSPC)
	require.ErrorIs(t, err, storeUnreachable)
	assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
	failed := h.events(restoreAttemptFailedReason)
	require.Len(t, failed, 1)
	assert.Contains(t, failed[0], "not retried: save restore attempt record")
}

// A pass runs every destination of the Pod. While one waits for its retry,
// one that already ended reports the same result without another restore.
func TestRestorePodContainersReportsAFinishedDestinationWhileASiblingWaits(t *testing.T) {
	pod := multiRestorePod()
	pod.Annotations[podcontract.RestoreRetryLimitAnnotation] = "1"
	w := makeTestController(t, pod)
	w.config.Restore.TimeoutSeconds = 600
	w.runtime = &fakeRuntime{resolveContainerPID: 4242, podUID: "host-uid"}
	volume := newFakeControlVolume(w)
	now := time.Now().UTC().Truncate(time.Second)
	started := now
	w.nowFn = func() time.Time { return now }
	var mu sync.Mutex
	restores := map[string]int{}
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		mu.Lock()
		defer mu.Unlock()
		restores[req.DestinationContainerName]++
		if req.DestinationContainerName == "engine-1" && restores["engine-1"] == 1 {
			return executor.RestoreResult{}, untouched(storeUnreachable)
		}
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 77}, nil
	}
	plan := &restorePlan{
		artifact: &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"},
		mappings: []podcontract.ContainerMapping{
			{Source: "main", Destination: "engine-0"},
			{Source: "main", Destination: "engine-1"},
		},
	}
	clientset := w.clientset.(*fake.Clientset)
	statusApplies := func() []string {
		var payloads []string
		for _, action := range clientset.Actions() {
			if patch, ok := action.(clientgotesting.PatchAction); ok && patch.GetSubresource() == "status" {
				payloads = append(payloads, string(patch.GetPatch()))
			}
		}
		return payloads
	}
	pass := func() bool {
		return w.restorePodContainers(context.Background(), pod, plan, "inference/restore-worker")
	}

	require.True(t, pass(), "a pass with a retry waiting did not ask for another")
	assert.Equal(t, map[string]int{"engine-0": 1, "engine-1": 1}, restores)
	waiting := statusApplies()
	assert.Contains(t, waiting[len(waiting)-1], "1 succeeded, 0 failed, 1 pending (engine-1)")

	now = started.Add(10 * time.Second)
	require.True(t, pass())
	assert.Equal(t, map[string]int{"engine-0": 1, "engine-1": 1}, restores, "a waiting pass restored a container")
	again := statusApplies()
	require.Len(t, again, len(waiting)+1, "a waiting pass wrote the condition more than once")
	assert.Equal(t, waiting[len(waiting)-1], again[len(again)-1], "a waiting pass changed the condition, so its own write starts another pass")

	now = started.Add(30 * time.Second)
	require.False(t, pass())
	assert.Equal(t, map[string]int{"engine-0": 1, "engine-1": 2}, restores)
	final := statusApplies()
	assert.Contains(t, final[len(final)-1], podcontract.RestoreReasonSucceeded)
	assert.Len(t, eventsForReason(clientset, restoreRequestedReason), 3)
	assert.Len(t, eventsForReason(clientset, restoreAttemptFailedReason), 1)
	assert.Equal(t, 2, volume.record(t, "host-uid", "engine-1").Attempt)
	assert.Equal(t, 1, volume.record(t, "host-uid", "engine-0").Attempt)
}
