// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"errors"
	"fmt"
	"testing"
)

// Decides whether a failed commit destroys recoverable staged data.
func TestTransactionGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain transport error", errors.New("connection reset"), false},
		{"storage error may succeed later", &FailureError{code: Failure_STORAGE_ERROR}, false},
		{"internal error may succeed later", &FailureError{code: Failure_INTERNAL_ERROR}, false},
		{"outcome unknown may already be published", &FailureError{code: Failure_OUTCOME_UNKNOWN}, false},
		{"broker unavailable may come back", &FailureError{code: Failure_UNAVAILABLE}, false},
		{"conflict is terminal but the broker still holds it", &FailureError{code: Failure_TRANSACTION_CONFLICT}, false},
		{"not found", &FailureError{code: Failure_TRANSACTION_NOT_FOUND}, true},
		{"expired", &FailureError{code: Failure_TRANSACTION_EXPIRED}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TransactionGone(tc.err); got != tc.want {
				t.Errorf("TransactionGone() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTransactionGoneSeesThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("commit PageBroker checkpoint: %w", &FailureError{code: Failure_TRANSACTION_EXPIRED})
	if !TransactionGone(wrapped) {
		t.Error("wrapped TRANSACTION_EXPIRED not recognised")
	}
}

func TestIgnoreAlreadyReleased(t *testing.T) {
	if err := ignoreAlreadyReleased(&FailureError{code: Failure_TRANSACTION_NOT_FOUND}); err != nil {
		t.Errorf("TRANSACTION_NOT_FOUND should be success, got %v", err)
	}
	if err := ignoreAlreadyReleased(&FailureError{code: Failure_TRANSACTION_EXPIRED}); err != nil {
		t.Errorf("TRANSACTION_EXPIRED should be success, got %v", err)
	}
	kept := &FailureError{code: Failure_STORAGE_ERROR}
	if err := ignoreAlreadyReleased(kept); !errors.Is(err, error(kept)) {
		t.Errorf("STORAGE_ERROR should be returned, got %v", err)
	}
	if err := ignoreAlreadyReleased(nil); err != nil {
		t.Errorf("nil should stay nil, got %v", err)
	}
}
