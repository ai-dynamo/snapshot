// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	restoreAttemptFailedReason    = "RestoreAttemptFailed"
	restoreRetriesExhaustedReason = "RestoreRetriesExhausted"

	// A pending restore gets a pass at least every restoreSafetyRequeueInterval,
	// so a shorter backoff would still wait that long.
	restoreRetryInitialBackoff = 30 * time.Second
	restoreRetryMaxBackoff     = 5 * time.Minute
)

// errRetryScheduled is a restore whose next attempt a later pass starts.
var errRetryScheduled = errors.New("the restore waits for its next attempt")

// errContainerStopped ends an attempt whose container the NRI hook reported
// stopped.
var errContainerStopped = errors.New("the container stopped during the attempt")

// errRestoreAlreadyFailed is a destination whose restore-failed an earlier
// pass wrote.
var errRestoreAlreadyFailed = errors.New("the restore already failed in an earlier pass")

type recovery int

const (
	endRestore recovery = iota
	retriesExhausted
	retryInPlace
)

// budget is what a restore has left after a failed attempt.
type budget struct {
	attempts int
	// time runs to the deadline, less the wait before the retry.
	time time.Duration
}

// finalCodes name the artifact or the request, so no attempt changes them.
// A restore reads them by what they mean to a restore: TRANSACTION_NOT_FOUND
// ends a capture, but a restore only loses a handle on a durable artifact.
var finalCodes = map[pagebroker.Failure_Code]bool{
	pagebroker.Failure_ARTIFACT_NOT_FOUND:   true,
	pagebroker.Failure_ARTIFACT_CORRUPT:     true,
	pagebroker.Failure_UNSUPPORTED_ARTIFACT: true,
	pagebroker.Failure_ACCESS_DENIED:        true,
	pagebroker.Failure_STORE_MISMATCH:       true,
	pagebroker.Failure_INVALID_REQUEST:      true,
}

func failureCode(err error) pagebroker.Failure_Code {
	var failure *pagebroker.FailureError
	if errors.As(err, &failure) {
		return failure.Code()
	}
	return pagebroker.Failure_UNSPECIFIED
}

func chooseRecovery(f *executor.RestoreFailure, ctxErr error, left budget) recovery {
	switch {
	case ctxErr != nil, f.Final, finalCodes[failureCode(f.Cause)], f.ContainerChanged:
		return endRestore
	case left.attempts <= 0 || left.time <= 0:
		return retriesExhausted
	default:
		return retryInPlace
	}
}

func restoreRetryBackoff(attempt int) time.Duration {
	backoff := restoreRetryInitialBackoff
	for i := 1; i < attempt && backoff < restoreRetryMaxBackoff; i++ {
		backoff *= 2
	}
	return min(backoff, restoreRetryMaxBackoff)
}

func budgetLeft(record *restoreAttempts, now time.Time) budget {
	left := budget{attempts: record.Limit + 1 - record.Attempt, time: time.Duration(math.MaxInt64)}
	if !record.Deadline.IsZero() {
		left.time = record.Deadline.Sub(now) - restoreRetryBackoff(record.Attempt)
	}
	return left
}

// restoreFailureOf reads a failed attempt's error as Restore returns it. Any
// other error, such as one from a replaced restore function, is a failure
// that left the container untouched.
func restoreFailureOf(err error) *executor.RestoreFailure {
	var failure *executor.RestoreFailure
	if errors.As(err, &failure) {
		return failure
	}
	failure = &executor.RestoreFailure{Step: executor.StepOf(err), Cause: err}
	var stepErr *executor.StepError
	if errors.As(err, &stepErr) {
		failure.Cause = stepErr.Err
	}
	return failure
}

// attemptFailed chooses what follows a failed attempt. A retry in the same
// container is saved in the record for a later pass to start; anything else
// ends the restore, including a failure that changed the container. An agent
// that is stopping chooses nothing: it may stop halfway through, and the next
// agent finds the attempt unfinished instead.
func (op *restoreOperation) attemptFailed(ctx, restoreCtx context.Context, record *restoreAttempts, err error) error {
	if ctx.Err() != nil {
		op.log.Info("Agent stopping; leaving the restore attempt to the next agent", "error", err.Error())
		return errRestoreInterrupted
	}
	failure := restoreFailureOf(err)
	if !failure.ContainerChanged && record.Limit > 0 && op.containerStopped(ctx) {
		failure = restoreFailureOf(stoppedDuringAttempt(failure))
		err = failure
	}
	now := op.controller.nowFn()
	left := budgetLeft(record, now)
	recovery := chooseRecovery(failure, restoreCtx.Err(), left)
	switch recovery {
	case retryInPlace:
		backoff := restoreRetryBackoff(record.Attempt)
		record.RetryAt = now.Add(backoff)
		record.LastFailure = failureSummary(failure)
		if saveErr := op.writeRestoreAttempts(op.nodeUID, record); saveErr != nil {
			op.reportFailedAttempt(ctx, record, failure, "not retried: "+saveErr.Error())
			return op.failRestore(ctx, errors.Join(err, saveErr))
		}
		op.reportFailedAttempt(ctx, record, failure, fmt.Sprintf("retrying in place in %s", backoff))
		return errRetryScheduled
	case retriesExhausted:
		op.reportFailedAttempt(ctx, record, failure, "no retries left")
		err = op.failRestore(ctx, op.retriesExhausted(ctx, record, left.attempts > 0, err))
	default:
		op.reportFailedAttempt(ctx, record, failure, "not retried: "+whyNotRetried(failure, restoreCtx.Err()))
		err = op.failRestore(ctx, err)
	}
	op.recordLastFailure(record, failure)
	return err
}

// recordLastFailure saves the failure that ended the restore for the retry
// status. It follows restore-failed: a record saved first would make a crash
// between the two look like an unfinished attempt, which a pass runs again.
func (op *restoreOperation) recordLastFailure(record *restoreAttempts, f *executor.RestoreFailure) {
	record.LastFailure = failureSummary(f)
	if err := op.writeRestoreAttempts(op.nodeUID, record); err != nil {
		op.log.Error(err, "Failed to save the failed attempt in the restore attempt record")
	}
}

// alreadyFailed reports a destination whose restore-failed an earlier pass
// wrote, with the failure that ended it when the record kept one.
func (op *restoreOperation) alreadyFailed() error {
	record, found, err := op.readRestoreAttempts(op.nodeUID)
	if err != nil || !found || record.LastFailure == nil {
		return errRestoreAlreadyFailed
	}
	last := &executor.RestoreFailure{Step: executor.Step(record.LastFailure.Step), Cause: errors.New(record.LastFailure.Cause)}
	return fmt.Errorf("%w: %w", errRestoreAlreadyFailed, last)
}

// snapshotUnusableCodes are the final codes that name the snapshot. The others
// name this node's access or configuration, or the request.
var snapshotUnusableCodes = map[pagebroker.Failure_Code]bool{
	pagebroker.Failure_ARTIFACT_NOT_FOUND:   true,
	pagebroker.Failure_ARTIFACT_CORRUPT:     true,
	pagebroker.Failure_UNSUPPORTED_ARTIFACT: true,
}

// failureSummary is a failed attempt as the retry status annotation reports
// it. The cause comes from beneath the step, which the summary names apart.
func failureSummary(f *executor.RestoreFailure) *podcontract.FailureSummary {
	return &podcontract.FailureSummary{
		Step:             string(f.Step),
		Cause:            truncateUTF8(f.Cause.Error(), podcontract.FailureCauseMaxBytes),
		SnapshotUnusable: snapshotUnusableCodes[failureCode(f.Cause)],
	}
}

func whyNotRetried(f *executor.RestoreFailure, ctxErr error) string {
	switch {
	case ctxErr != nil:
		return "the restore timed out"
	case f.Final:
		return "no other attempt can succeed"
	case finalCodes[failureCode(f.Cause)]:
		return fmt.Sprintf("PageBroker reported %s", failureCode(f.Cause))
	default:
		return "another attempt needs a new container"
	}
}

// containerStopped reports whether the attempt's container no longer runs,
// which the attempt's own error may not show.
func (op *restoreOperation) containerStopped(ctx context.Context) bool {
	running, err := op.controller.runtime.ContainerRunning(ctx, op.containerID)
	if err != nil {
		op.log.V(1).Info("Cannot read the container's state; taking it as running", "error", err.Error())
		return false
	}
	return !running
}

// watchContainer returns the attempt's context, which RestoreContainerStopped
// ends so that an attempt does not run on for a container that is gone.
// endAttempt reports whether that happened. A restore that allows no retries
// runs as before.
func (op *restoreOperation) watchContainer(ctx context.Context, record *restoreAttempts) (attemptCtx context.Context, endAttempt func() bool) {
	if record.Limit == 0 {
		return ctx, func() bool { return false }
	}
	attemptCtx, cancel := context.WithCancelCause(ctx)
	op.controller.runningAttempts.Store(op.containerID, cancel)
	return attemptCtx, func() bool {
		op.controller.runningAttempts.Delete(op.containerID)
		stopped := errors.Is(context.Cause(attemptCtx), errContainerStopped)
		cancel(nil)
		return stopped
	}
}

// stoppedDuringAttempt names the stop as the failure's cause, since the
// attempt itself saw only its context end.
func stoppedDuringAttempt(err error) error {
	stopped := *restoreFailureOf(err)
	stopped.Cause = fmt.Errorf("%w: %w", errContainerStopped, stopped.Cause)
	stopped.ContainerChanged = true
	return &stopped
}

// reportFailedAttempt emits RestoreAttemptFailed for a restore that allows
// retries. With a limit of 0 the restore fails as it always has.
func (op *restoreOperation) reportFailedAttempt(ctx context.Context, record *restoreAttempts, f *executor.RestoreFailure, recovery string) {
	op.log.Info("Restore attempt failed", "step", string(f.Step), "error", f.Error(), "recovery", recovery)
	if record.Limit == 0 {
		return
	}
	emitPodEvent(ctx, op.controller.clientset, op.log, op.pod, snapshotEventComponent, corev1.EventTypeWarning, restoreAttemptFailedReason,
		fmt.Sprintf("Attempt %d of %d for destination %s failed: %s; %s", record.Attempt, record.Limit+1, op.destination, f.Error(), recovery))
}

// retriesExhausted emits RestoreRetriesExhausted for a restore that allowed
// retries and returns the error that ends it. A restore can run out of
// attempts or, with attempts left, out of time.
func (op *restoreOperation) retriesExhausted(ctx context.Context, record *restoreAttempts, outOfTime bool, cause error) error {
	if record.Limit == 0 {
		return cause
	}
	err := fmt.Errorf("restore used all %d attempts: %w", record.Attempt, cause)
	if outOfTime {
		err = fmt.Errorf("restore ran out of time after attempt %d of %d: %w", record.Attempt, record.Limit+1, cause)
	}
	emitPodEvent(ctx, op.controller.clientset, op.log, op.pod, snapshotEventComponent, corev1.EventTypeWarning, restoreRetriesExhaustedReason,
		fmt.Sprintf("Destination %s: %s", op.destination, err.Error()))
	return err
}
