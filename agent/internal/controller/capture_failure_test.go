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
