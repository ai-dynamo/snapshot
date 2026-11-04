// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
)

// Reasons recorded on a failed work order. The dump ones separate the
// conditions an operator acts on differently: waiting for node staging to free
// up, fixing a request the broker refuses, and a capture whose staged data is
// already gone.
const (
	reasonCheckpointFailed   = "CheckpointFailed"
	reasonCheckpointTimeout  = "CheckpointTimeout"
	reasonStagingUnavailable = "StagingUnavailable"
	reasonStagedDataLost     = "StagedDataLost"
	reasonBrokerRejected     = "BrokerRejected"
	// reasonInvalidCheckpointReadinessProbe fails a work order before the
	// dump: its probe can never run, such as an HTTP probe on a port name the
	// container doesn't have.
	reasonInvalidCheckpointReadinessProbe = "InvalidCheckpointReadinessProbe"

	// reasonRetriesExhausted separates a checkpoint that used up its retries from
	// one that failed on its first try.
	reasonRetriesExhausted = "RetriesExhausted"

	// reasonCheckpointAttemptFailed reports one failed attempt on the source Pod,
	// so an owner sees the retries rather than only the outcome.
	reasonCheckpointAttemptFailed = "CheckpointAttemptFailed"

	// reasonStoreExhausted is the destination store out of room, which no retry
	// on this node clears. Staging running out is reasonStagingUnavailable.
	reasonStoreExhausted = "StoreExhausted"
)

// captureFailure is how a failed attempt reads: the reason recorded on the work
// order, and whether another attempt could reach a different outcome.
type captureFailure struct {
	reason    string
	retryable bool
}

// classifyCaptureFailure decides both from the error alone. It cannot see which
// step failed, so it never concludes that a dump completed; a caller that knows
// the step can narrow this further.
func classifyCaptureFailure(err error) captureFailure {
	cause := captureCause(err)
	if errors.Is(cause, context.DeadlineExceeded) {
		return captureFailure{reason: reasonCheckpointTimeout}
	}
	var failure *pagebroker.FailureError
	if !errors.As(cause, &failure) {
		return captureFailure{reason: reasonCheckpointFailed}
	}
	switch failure.Code() {
	case pagebroker.Failure_INSUFFICIENT_STORAGE:
		// Which store ran out depends on where: the commit writes the artifact to
		// the destination, and no retry on this node frees room there.
		if executor.StepOf(err) == executor.StepCommitCheckpoint {
			return captureFailure{reason: reasonStoreExhausted}
		}
		// Refused before any bytes were written, so the node is momentarily full
		// rather than this image being too big for it.
		return captureFailure{reason: reasonStagingUnavailable, retryable: true}
	case pagebroker.Failure_STORAGE_ERROR, pagebroker.Failure_INTERNAL_ERROR,
		pagebroker.Failure_UNAVAILABLE, pagebroker.Failure_STORAGE_UNAVAILABLE:
		// A destination that filled mid-write reports a generic storage error, so
		// the message is all that separates it from a store that is merely unwell.
		if executor.StepOf(err) == executor.StepCommitCheckpoint && isOutOfSpace(err) {
			return captureFailure{reason: reasonStoreExhausted}
		}
		return captureFailure{reason: reasonCheckpointFailed, retryable: true}
	case pagebroker.Failure_TRANSACTION_NOT_FOUND, pagebroker.Failure_TRANSACTION_EXPIRED:
		// The broker is not holding the transaction, so whatever was staged for it
		// is already gone and no attempt can publish it.
		return captureFailure{reason: reasonStagedDataLost}
	case pagebroker.Failure_INVALID_REQUEST, pagebroker.Failure_TRANSACTION_CONFLICT,
		pagebroker.Failure_STORE_MISMATCH, pagebroker.Failure_ACCESS_DENIED,
		pagebroker.Failure_UNSUPPORTED_ARTIFACT, pagebroker.Failure_ARTIFACT_CORRUPT:
		return captureFailure{reason: reasonBrokerRejected}
	default:
		return captureFailure{reason: reasonCheckpointFailed}
	}
}

// captureCause strips the cleanup failures the executor reports alongside a
// capture error, so a code from a failed Abort is not read as why the capture
// failed — an abort reporting TRANSACTION_NOT_FOUND would otherwise look like
// the staged data had been lost.
func captureCause(err error) error {
	for current := err; current != nil; current = errors.Unwrap(current) {
		joined, ok := current.(interface{ Unwrap() []error })
		if !ok {
			continue
		}
		for _, part := range joined.Unwrap() {
			if !executor.IsCleanupFailure(part) {
				return captureCause(part)
			}
		}
		// Nothing but cleanup failed, so the cleanup is the cause.
		return err
	}
	return err
}

// isOutOfSpace reports a full filesystem. PageBroker gives a full destination no
// code of its own, so for its errors the message text is all there is.
func isOutOfSpace(err error) bool {
	return errors.Is(err, unix.ENOSPC) ||
		strings.Contains(strings.ToLower(err.Error()), unix.ENOSPC.Error())
}
