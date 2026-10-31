// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateReason covers the Kubernetes condition-reason rule.
func TestValidateReason(t *testing.T) {
	for _, reason := range []string{"A", "MemberFailed", "Refused_1", "a:b,c"} {
		require.NoError(t, ValidateReason(reason), "reason %q", reason)
	}
	for _, reason := range []string{"", "1Refused", "has space", "Trailing:", "Trailing,", "dash-ed",
		"A" + strings.Repeat("a", maxReasonLength)} {
		require.ErrorIs(t, ValidateReason(reason), ErrInvalid, "reason %q", reason)
	}
}

// TestAbortError covers matching aborts by reason.
func TestAbortError(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &AbortError{Reason: ReasonCoordinatorLost})
	require.ErrorIs(t, err, ErrCoordinatorLost)
	require.NotErrorIs(t, &AbortError{Reason: ReasonMemberFailed}, ErrCoordinatorLost)
	require.ErrorIs(t, EndedError(nil), ErrClosed)
	require.ErrorIs(t, EndedError([]byte("Refused")), &AbortError{Reason: "Refused"})
}
