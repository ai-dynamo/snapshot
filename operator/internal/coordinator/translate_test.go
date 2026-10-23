// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/internal/clocktest"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
)

func TestEveryErrorAnOperationReportsHasItsCode(t *testing.T) {
	tests := []struct {
		err    error
		code   wire.Code
		reason string
	}{
		{err: session.ErrNotFound, code: wire.Code_CODE_NOT_FOUND},
		{err: session.ErrNotYet, code: wire.Code_CODE_NOT_YET},
		{err: session.ErrExists, code: wire.Code_CODE_EXISTS},
		{err: session.ErrMismatch, code: wire.Code_CODE_MISMATCH},
		{err: session.ErrNotCounter, code: wire.Code_CODE_NOT_COUNTER},
		{err: session.ErrBarrierCount, code: wire.Code_CODE_BARRIER_COUNT},
		{err: &session.AbortError{Reason: session.ReasonMemberFailed}, code: wire.Code_CODE_ABORTED,
			reason: "MemberFailed"},
		{err: session.ErrClosed, code: wire.Code_CODE_CLOSED},
		{err: session.ErrNotMember, code: wire.Code_CODE_NOT_MEMBER},
		{err: session.ErrForbidden, code: wire.Code_CODE_FORBIDDEN},
		{err: session.ErrTooLarge, code: wire.Code_CODE_TOO_LARGE},
		{err: session.ErrTooMany, code: wire.Code_CODE_TOO_MANY},
		{err: session.ErrInvalid, code: wire.Code_CODE_INVALID},
		{err: context.Canceled, code: wire.Code_CODE_CANCELLED},
		{err: context.DeadlineExceeded, code: wire.Code_CODE_CANCELLED},
		{err: errors.New("the backend is unreachable"), code: wire.Code_CODE_INTERNAL},
	}
	for _, tt := range tests {
		t.Run(tt.err.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("key %q: %w", "snapshot/handles", tt.err)

			e := errorOf(wrapped)

			assert.Equal(t, tt.code, e.GetCode())
			assert.Equal(t, tt.reason, e.GetReason())
			assert.Equal(t, wrapped.Error(), e.GetMessage())
		})
	}
}

func TestEveryRoleHasItsCaller(t *testing.T) {
	tests := []struct {
		name   string
		origin *wire.Origin
		want   session.Caller
	}{
		{name: "agent", origin: &wire.Origin{Role: wire.Role_ROLE_AGENT},
			want: session.Caller{Member: memberA, Role: session.RoleAgent}},
		{name: "relay", origin: &wire.Origin{Role: wire.Role_ROLE_RELAY},
			want: session.Caller{Member: memberA, Role: session.RoleRelay}},
		{
			name:   "library",
			origin: &wire.Origin{Role: wire.Role_ROLE_LIBRARY, Library: "cudart", Version: "13.0"},
			want:   session.Caller{Member: memberA, Role: session.RoleLibrary, Library: "cudart", Version: "13.0"},
		},
		{
			name:   "a library field on another role",
			origin: &wire.Origin{Role: wire.Role_ROLE_AGENT, Library: "cudart", Version: "13.0"},
			want:   session.Caller{Member: memberA, Role: session.RoleAgent},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller, err := callerOf(memberA, tt.origin)

			require.NoError(t, err)
			assert.Equal(t, tt.want, caller)
		})
	}
}

func TestAnOriginWithoutARoleIsInvalid(t *testing.T) {
	for _, o := range []*wire.Origin{nil, {}, {Role: wire.Role(99)}} {
		_, err := callerOf(memberA, o)

		require.ErrorIs(t, err, session.ErrInvalid)
	}
}

func TestEndedCarriesHowTheSessionEnded(t *testing.T) {
	closed := endedMessage(session.ErrClosed).GetEnded().GetError()
	aborted := endedMessage(&session.AbortError{Reason: session.ReasonDeadlineExceeded}).GetEnded().GetError()

	assert.Equal(t, wire.Code_CODE_CLOSED, closed.GetCode())
	assert.Empty(t, closed.GetReason())
	assert.Equal(t, wire.Code_CODE_ABORTED, aborted.GetCode())
	assert.Equal(t, "DeadlineExceeded", aborted.GetReason())
}

func TestADeadlineOnTheCallersContextIsCancelled(t *testing.T) {
	manager, err := session.NewManager(session.Config{Store: memory.New(), Clock: clocktest.New(baseTime)})
	require.NoError(t, err)
	s, err := manager.Ensure(session.Spec{
		UID: attempt, Members: []session.Member{{ID: memberA}}, Deadline: baseTime.Add(deadlineIn),
	})
	require.NoError(t, err)
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()

	_, err = s.Get(ctx, session.Caller{Member: memberA, Role: session.RoleAgent}, agentKey, deadlineIn)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, wire.Code_CODE_CANCELLED, errorOf(err).GetCode(), "a stream's deadline is not an internal error")
}

func TestAWaitIsZeroWhenAbsentAndRefusedWhenNegativeOrMalformed(t *testing.T) {
	absent, err := waitOf(nil)
	require.NoError(t, err)
	assert.Zero(t, absent)
	_, err = waitOf(durationpb.New(-time.Nanosecond))
	require.ErrorIs(t, err, session.ErrInvalid)
	_, err = waitOf(&durationpb.Duration{Seconds: -1, Nanos: 1})
	require.ErrorIs(t, err, session.ErrInvalid)
}
