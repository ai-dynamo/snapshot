// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
)

// What an operator does about a failed capture differs by reason, and whether a
// retry can help differs again. Both are decided here.
func TestClassifyCaptureFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		wantReason    string
		wantRetryable bool
	}{
		{"an unrecognised error says no more than it knows", errors.New("criu exited 1"), reasonCheckpointFailed, false},
		{"the capture ran out of time", context.DeadlineExceeded, reasonCheckpointTimeout, false},
		{"staging refused under contention frees up", pagebroker.NewFailureError(pagebroker.Failure_INSUFFICIENT_STORAGE, ""), reasonStagingUnavailable, true},
		{"a storage error may clear", pagebroker.NewFailureError(pagebroker.Failure_STORAGE_ERROR, ""), reasonCheckpointFailed, true},
		{"an internal error may clear", pagebroker.NewFailureError(pagebroker.Failure_INTERNAL_ERROR, ""), reasonCheckpointFailed, true},
		{"the broker may come back", pagebroker.NewFailureError(pagebroker.Failure_UNAVAILABLE, ""), reasonCheckpointFailed, true},
		{"the store may come back", pagebroker.NewFailureError(pagebroker.Failure_STORAGE_UNAVAILABLE, ""), reasonCheckpointFailed, true},
		{"a conflicting request never succeeds", pagebroker.NewFailureError(pagebroker.Failure_TRANSACTION_CONFLICT, ""), reasonBrokerRejected, false},
		{"a malformed request never succeeds", pagebroker.NewFailureError(pagebroker.Failure_INVALID_REQUEST, ""), reasonBrokerRejected, false},
		{"the wrong store never succeeds", pagebroker.NewFailureError(pagebroker.Failure_STORE_MISMATCH, ""), reasonBrokerRejected, false},
		{"the staged data is gone", pagebroker.NewFailureError(pagebroker.Failure_TRANSACTION_NOT_FOUND, ""), reasonStagedDataLost, false},
		{"the transaction expired", pagebroker.NewFailureError(pagebroker.Failure_TRANSACTION_EXPIRED, ""), reasonStagedDataLost, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCaptureFailure(tc.err)
			if got.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.reason, tc.wantReason)
			}
			if got.retryable != tc.wantRetryable {
				t.Errorf("retryable = %v, want %v", got.retryable, tc.wantRetryable)
			}
		})
	}
}

func TestClassifyCaptureFailureSeesThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("commit PageBroker checkpoint: %w", pagebroker.NewFailureError(pagebroker.Failure_UNAVAILABLE, ""))
	if got := classifyCaptureFailure(wrapped); !got.retryable {
		t.Errorf("wrapped UNAVAILABLE classified as %+v", got)
	}
}

// The executor joins the cleanup failure onto the capture failure. A dump that
// failed without a broker code leaves the Abort's code the only one in the
// tree, so classifying the join reads the cleanup as the reason: a CRIU failure
// would be recorded as staged data lost.
func TestClassifyCaptureFailureIgnoresTheCleanupError(t *testing.T) {
	dump := errors.New("criu dump: frozen task is unreachable")
	abort := fmt.Errorf("abort PageBroker checkpoint %q: %w", "tx-1",
		pagebroker.NewFailureError(pagebroker.Failure_TRANSACTION_NOT_FOUND, ""))

	// How executorCheckpoint presents it: the join, wrapped once more.
	joined := fmt.Errorf("checkpoint: %w", errors.Join(dump, abort))

	if got := classifyCaptureFailure(joined); got.reason != reasonCheckpointFailed {
		t.Errorf("classifyCaptureFailure() = %+v, want %q from the dump", got, reasonCheckpointFailed)
	}
}

// The same join, with a code on both sides: the dump's is the one that counts.
func TestClassifyCaptureFailurePrefersTheDumpsCode(t *testing.T) {
	dump := pagebroker.NewFailureError(pagebroker.Failure_STORAGE_ERROR, "storage I/O failed")
	abort := fmt.Errorf("abort PageBroker checkpoint %q: %w", "tx-1",
		pagebroker.NewFailureError(pagebroker.Failure_TRANSACTION_NOT_FOUND, ""))

	got := classifyCaptureFailure(fmt.Errorf("checkpoint: %w", errors.Join(dump, abort)))
	if got.reason != reasonCheckpointFailed || !got.retryable {
		t.Errorf("classifyCaptureFailure() = %+v, want the dump's retryable STORAGE_ERROR", got)
	}
}

// When the capture itself succeeded and only cleanup failed, errors.Join has
// dropped the nil, so the first branch is the cleanup — and that is the cause.
func TestClassifyCaptureFailureUsesCleanupWhenItIsTheOnlyFailure(t *testing.T) {
	abort := fmt.Errorf("abort PageBroker checkpoint %q: %w", "tx-1",
		pagebroker.NewFailureError(pagebroker.Failure_TRANSACTION_NOT_FOUND, ""))

	got := classifyCaptureFailure(fmt.Errorf("checkpoint: %w", errors.Join(nil, abort)))
	if got.reason != reasonStagedDataLost {
		t.Errorf("classifyCaptureFailure() = %+v, want %q", got, reasonStagedDataLost)
	}
}

func TestCaptureCauseLeavesAPlainErrorIntact(t *testing.T) {
	plain := pagebroker.NewFailureError(pagebroker.Failure_UNAVAILABLE, "")
	if got := captureCause(fmt.Errorf("checkpoint: %w", plain)); !errors.Is(got, error(plain)) {
		t.Errorf("captureCause() dropped the chain of an unjoined error: %v", got)
	}
}
