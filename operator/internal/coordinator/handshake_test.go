// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"testing"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAttachWelcomesAMember(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)

	_, welcome := f.attach(t, memberB, 0)

	assert.Equal(t, f.epoch(), welcome.GetEpoch())
	assert.Equal(t, int32(1), welcome.GetRank())
	require.Len(t, welcome.GetMembers(), 2)
	assert.Equal(t, string(memberA), welcome.GetMembers()[0].GetId())
	assert.Equal(t, int32(0), welcome.GetMembers()[0].GetRank())
	assert.Equal(t, string(memberB), welcome.GetMembers()[1].GetId())
	assert.Equal(t, int32(1), welcome.GetMembers()[1].GetRank())
	assert.Equal(t, baseTime.Add(deadlineIn), welcome.GetDeadline().AsTime())
	assert.Equal(t, []memberKey{{uid: attempt, member: memberB}}, f.auth.asked)
}

func TestAMalformedHandshakeIsInvalidArgument(t *testing.T) {
	tests := []struct {
		name      string
		misbehave func(*testing.T, *fixture) *client
	}{
		{name: "a request before Hello", misbehave: func(t *testing.T, f *fixture) *client {
			c := f.open(t)
			c.request(t, asAgent().insert("r-1", agentKey, []byte("value")))
			return c
		}},
		{name: "a Hello without a session", misbehave: func(t *testing.T, f *fixture) *client {
			c := f.open(t)
			c.send(t, &wire.ClientMessage{Message: &wire.ClientMessage_Hello{Hello: &wire.Hello{Member: string(memberA)}}})
			return c
		}},
		{name: "a Hello without a member", misbehave: func(t *testing.T, f *fixture) *client {
			c := f.open(t)
			c.send(t, &wire.ClientMessage{Message: &wire.ClientMessage_Hello{Hello: &wire.Hello{Session: string(attempt)}}})
			return c
		}},
		{name: "the stream closing before Hello", misbehave: func(t *testing.T, f *fixture) *client {
			c := f.open(t)
			require.NoError(t, c.stream.CloseSend())
			return c
		}},
		{name: "a second Hello", misbehave: func(t *testing.T, f *fixture) *client {
			c, _ := f.attach(t, memberA, 0)
			c.send(t, hello(memberA, 0))
			return c
		}},
		{name: "a message with nothing in it", misbehave: func(t *testing.T, f *fixture) *client {
			c, _ := f.attach(t, memberA, 0)
			c.send(t, &wire.ClientMessage{})
			return c
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.ensure(t)

			c := tt.misbehave(t, f)

			assert.Equal(t, codes.InvalidArgument, status.Code(c.end(t)))
		})
	}
}

func TestAnAuthorizerErrorNeverTouchesTheSession(t *testing.T) {
	tests := []struct {
		name    string
		refusal error
		want    codes.Code
		// passed reports whether the error reaches the client unchanged, rather
		// than as Unavailable, and is logged.
		passed bool
	}{
		{name: "Unauthenticated", refusal: status.Error(codes.Unauthenticated, "no token"),
			want: codes.Unauthenticated, passed: true},
		{name: "PermissionDenied", refusal: status.Error(codes.PermissionDenied, "not on this node"),
			want: codes.PermissionDenied, passed: true},
		{name: "Unavailable", refusal: status.Error(codes.Unavailable, "TokenReview failed"),
			want: codes.Unavailable, passed: true},
		{name: "NotFound", refusal: status.Error(codes.NotFound, "attempt not cached yet"),
			want: codes.NotFound, passed: true},
		{name: "a plain error", refusal: errors.New("the cache broke"), want: codes.Unavailable},
		{name: "another status", refusal: status.Error(codes.Aborted, "the cache broke"), want: codes.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.ensure(t)
			f.auth.refuse(tt.refusal)

			stale := f.open(t)
			// A stale epoch would fail the session if the server consulted it.
			stale.send(t, hello(memberA, f.epoch()+1))
			unknown := f.open(t)
			unknown.send(t, &wire.ClientMessage{Message: &wire.ClientMessage_Hello{Hello: &wire.Hello{
				Session: "attempt-2", Member: string(memberA),
			}}})

			for _, err := range []error{stale.end(t), unknown.end(t)} {
				assert.Equal(t, tt.want, status.Code(err))
				if tt.passed {
					assert.Equal(t, status.Convert(tt.refusal).Message(), status.Convert(err).Message())
				}
			}
			require.NoError(t, s.Err(), "the session was never touched")
			if tt.passed {
				assert.NotContains(t, f.logs.String(), "outside its contract")
			} else {
				assert.Contains(t, f.logs.String(), "outside its contract")
			}
		})
	}
}

func TestANonMemberIsPermissionDenied(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	c := f.open(t)

	c.send(t, hello(memberC, 0))

	assert.Equal(t, codes.PermissionDenied, status.Code(c.end(t)))
}

func TestAFirstAttachToAnUnknownSessionIsNotFound(t *testing.T) {
	f := newFixture(t)
	c := f.open(t)

	c.send(t, hello(memberA, 0))

	assert.Equal(t, codes.NotFound, status.Code(c.end(t)))
}

func TestAHelloToAnEndedSessionGetsEnded(t *testing.T) {
	tests := []struct {
		name   string
		end    func(*session.Session) error
		code   wire.Code
		reason session.Reason
	}{
		{
			name: "closed",
			end: func(s *session.Session) error {
				return s.Leave(context.Background(), session.Caller{Member: memberA, Role: session.RoleAgent})
			},
			code: wire.Code_CODE_CLOSED,
		},
		{
			name: "failed",
			end: func(s *session.Session) error {
				return s.Fail(context.Background(), session.Caller{Member: memberA, Role: session.RoleAgent}, "NodeLost")
			},
			code:   wire.Code_CODE_ABORTED,
			reason: "NodeLost",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			require.NoError(t, tt.end(f.ensure(t, memberA)))
			c := f.open(t)

			c.send(t, hello(memberA, 0))

			c.ended(t, tt.code, tt.reason)
		})
	}
}
