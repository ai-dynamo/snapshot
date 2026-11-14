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
// ends the restore, including a failure that changed the container.
func (op *restoreOperation) attemptFailed(ctx, restoreCtx context.Context, record *restoreAttempts, err error) error {
	failure := restoreFailureOf(err)
	now := op.controller.nowFn()
	left := budgetLeft(record, now)
	recovery := chooseRecovery(failure, restoreCtx.Err(), left)
	switch recovery {
	case retryInPlace:
		backoff := restoreRetryBackoff(record.Attempt)
		record.RetryAt = now.Add(backoff)
		if saveErr := op.writeRestoreAttempts(op.nodeUID, record); saveErr != nil {
			op.reportFailedAttempt(ctx, record, failure, "not retried: "+saveErr.Error())
			return op.failRestore(ctx, errors.Join(err, saveErr))
		}
		op.reportFailedAttempt(ctx, record, failure, fmt.Sprintf("retrying in place in %s", backoff))
		return errRetryScheduled
	case retriesExhausted:
		op.reportFailedAttempt(ctx, record, failure, "no retries left")
		return op.failRestore(ctx, op.retriesExhausted(ctx, record, left.attempts > 0, err))
	default:
		op.reportFailedAttempt(ctx, record, failure, "not retried: "+whyNotRetried(failure, restoreCtx.Err()))
		return op.failRestore(ctx, err)
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
