// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"errors"
	"fmt"
	"testing"
)

// Decides whether a failed commit keeps the staged pages or releases the node memory.
func TestCommitWorthRetrying(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("connection reset"), false},
		{"mid-exchange transport failure leaves the outcome unknown", transportError{cause: errors.New("EOF")}, true},
		{"dial failure never reached the broker", transportError{cause: errors.New("ENOENT"), dial: true}, false},
		{"storage error may clear", &FailureError{code: Failure_STORAGE_ERROR}, true},
		{"broker unavailable may come back", &FailureError{code: Failure_UNAVAILABLE}, true},
		{"storage transiently unavailable", &FailureError{code: Failure_STORAGE_UNAVAILABLE}, true},
		{"outcome unknown may already be published", &FailureError{code: Failure_OUTCOME_UNKNOWN}, true},
		{"conflict never succeeds", &FailureError{code: Failure_TRANSACTION_CONFLICT}, false},
		{"invalid request never succeeds", &FailureError{code: Failure_INVALID_REQUEST}, false},
		{"internal error asks for a new transaction", &FailureError{code: Failure_INTERNAL_ERROR}, false},
		{"staging is out of space", &FailureError{code: Failure_INSUFFICIENT_STORAGE}, false},
		{"access denied", &FailureError{code: Failure_ACCESS_DENIED}, false},
		{"not found has nothing left to hold", &FailureError{code: Failure_TRANSACTION_NOT_FOUND}, false},
		{"expired has nothing left to hold", &FailureError{code: Failure_TRANSACTION_EXPIRED}, false},
		{"unspecified is not a reason to hold", &FailureError{code: Failure_UNSPECIFIED}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CommitWorthRetrying(tc.err); got != tc.want {
				t.Errorf("CommitWorthRetrying() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCommitWorthRetryingSeesThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("commit PageBroker checkpoint: %w", &FailureError{code: Failure_STORAGE_ERROR})
	if !CommitWorthRetrying(wrapped) {
		t.Error("wrapped STORAGE_ERROR not recognised")
	}
}
