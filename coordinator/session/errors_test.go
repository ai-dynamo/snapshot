// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
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
}

// TestEndedError covers decoding the ended key, which any member may write:
// no bytes close the session, a valid reason fails it with that reason, and
// anything else fails it with ReasonInvalid.
func TestEndedError(t *testing.T) {
	require.ErrorIs(t, EndedError(nil), ErrClosed)
	require.ErrorIs(t, EndedError([]byte{}), ErrClosed)
	require.ErrorIs(t, EndedError([]byte("Refused")), &AbortError{Reason: "Refused"})
	for name, value := range map[string][]byte{
		"not a reason":                []byte("has space"),
		"binary":                      {0xff, 0x00},
		"one byte over a reason":      []byte("A" + strings.Repeat("a", maxReasonLength)),
		"the Snapshot bucket's value": bytes.Repeat([]byte("A"), MaxValue),
	} {
		require.ErrorIs(t, EndedError(value), &AbortError{Reason: ReasonInvalid}, name)
	}
}
