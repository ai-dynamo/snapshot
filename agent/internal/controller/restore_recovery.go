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

	restoreAttemptFailedMessage    = "Attempt %d of %d for destination %s failed: %s; %s"
	restoreRetriesExhaustedMessage = "Destination %s: %s"
	restoreRequestedAttemptSuffix  = " (attempt %d of %d)"

	recoveryRetryInPlace  = "retrying in place in %s"
	recoveryNoRetriesLeft = "no retries left"
	recoveryNotRetried    = "not retried: "

	notRetriedTimedOut         = "the restore timed out"
	notRetriedFinal            = "no other attempt can succeed"
	notRetriedPageBrokerCode   = "PageBroker reported %s"
	notRetriedContainerChanged = "another attempt needs a new container"

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

// chooseRecovery decides what follows a failed attempt. The restore ends when
// no retry can help: its deadline passed, the failure is final, PageBroker
// reported a fault in the artifact or the request, or the attempt changed the
// container. Otherwise it retries in the same container while attempts and
// time remain, and runs out of retries when they do not.
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
// ends the restore, including a failure that changed the container. A stop
// changed it, whether it was reported during the attempt, as stopped says, or
// the runtime shows it now. An agent that is stopping chooses nothing: it may
// stop halfway through, and the next agent finds the attempt unfinished
// instead.
func (op *restoreOperation) attemptFailed(ctx, restoreCtx context.Context, record *restoreAttempts, err error, stopped bool) error {
	if ctx.Err() != nil {
		op.log.Info("Agent stopping; leaving the restore attempt to the next agent", "error", err.Error())
		return errRestoreInterrupted
	}
	failure := restoreFailureOf(err)
	if stopped || (!failure.ContainerChanged && record.Limit > 0 && !op.containerRunning(ctx)) {
		failure = stoppedDuringAttempt(failure)
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
			op.reportFailedAttempt(ctx, record, failure, recoveryNotRetried+saveErr.Error())
			return op.failRestore(ctx, errors.Join(err, saveErr))
		}
		op.reportFailedAttempt(ctx, record, failure, fmt.Sprintf(recoveryRetryInPlace, backoff))
		return errRetryScheduled
	case retriesExhausted:
		op.reportFailedAttempt(ctx, record, failure, recoveryNoRetriesLeft)
		err = op.failRestore(ctx, op.retriesExhausted(ctx, record, left.attempts > 0, err))
	default:
		op.reportFailedAttempt(ctx, record, failure, recoveryNotRetried+whyNotRetried(failure, restoreCtx.Err()))
		err = op.failRestore(ctx, err)
	}
	op.recordLastFailure(record, failure)
	return err
}

// recordLastFailure saves the failure that ended the restore for the retry
// status. It follows restore-failed: a record saved first would make a crash
// between the two look like an unfinished attempt, which a pass runs again.
// A restore without retries keeps no record.
func (op *restoreOperation) recordLastFailure(record *restoreAttempts, f *executor.RestoreFailure) {
	if record.Limit == 0 {
		return
	}
	record.LastFailure = failureSummary(f)
	if err := op.writeRestoreAttempts(op.nodeUID, record); err != nil {
		op.log.Error(err, "Failed to save the failed attempt in the restore attempt record")
	}
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
		return notRetriedTimedOut
	case f.Final:
		return notRetriedFinal
	case finalCodes[failureCode(f.Cause)]:
		return fmt.Sprintf(notRetriedPageBrokerCode, failureCode(f.Cause))
	default:
		return notRetriedContainerChanged
	}
}

// containerRunning reports whether the attempt's container still runs, which
// the attempt's own error may not show.
func (op *restoreOperation) containerRunning(ctx context.Context) bool {
	running, err := op.controller.runtime.ContainerRunning(ctx, op.containerID)
	if err != nil {
		op.log.V(1).Info("Cannot read the container's state; taking it as running", "error", err.Error())
		return true
	}
	return running
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
func stoppedDuringAttempt(f *executor.RestoreFailure) *executor.RestoreFailure {
	stopped := *f
	stopped.Cause = fmt.Errorf("%w: %w", errContainerStopped, f.Cause)
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
		fmt.Sprintf(restoreAttemptFailedMessage, record.Attempt, record.Limit+1, op.destination, f.Error(), recovery))
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
		fmt.Sprintf(restoreRetriesExhaustedMessage, op.destination, err.Error()))
	return err
}
