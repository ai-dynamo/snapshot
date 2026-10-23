// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"strings"
	"testing"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// used returns the bytes a session holds against its budget.
func used(s *Session) int64 { return s.ledger.budget.used.Load() }

// fill sets what a session holds, so a test can stand at the budget's edge
// without writing up to it.
func fill(s *Session, n int64) { s.ledger.budget.used.Store(n) }

func TestTheBudgetChargesWhatEachEntryCosts(t *testing.T) {
	const key, participant = agentKey, "pid-17"
	value := []byte("a value of some length")
	tests := []struct {
		name  string
		write func(context.Context, *Session) error
		want  int64
	}{
		{
			name:  "a key and its value",
			write: func(ctx context.Context, s *Session) error { return s.Insert(ctx, requestOf(memberA, ""), key, value) },
			want:  cost(keyOverhead, len(key), len(value)),
		},
		{
			name: "a counter",
			write: func(ctx context.Context, s *Session) error {
				_, err := s.Increment(ctx, requestOf(memberA, ""), key)
				return err
			},
			want: cost(keyOverhead, len(key), kv.CounterSize),
		},
		{
			name: "a barrier, when it is created",
			write: func(ctx context.Context, s *Session) error {
				return s.CreateBarrier(ctx, requestOf(memberA, ""), barrierName, 2)
			},
			want: cost(barrierOverhead, len(barrierName)),
		},
		{
			name: "a barrier and a participant, when a post names both",
			write: func(ctx context.Context, s *Session) error {
				return s.PostBarrier(ctx, Request{Caller: ncclOf(memberA)}, ncclOther, participant)
			},
			want: cost(barrierOverhead, len(ncclOther)) + cost(participantOverhead, len(participant)),
		},
		{
			name: "a recorded request, beside what it writes",
			write: func(ctx context.Context, s *Session) error {
				return s.Insert(ctx, requestOf(memberA, "request-1"), key, value)
			},
			want: cost(requestOverhead, len("request-1"), len(memberA)) + cost(keyOverhead, len(key), len(value)),
		},
		{
			name: "a recorded request, with every string its caller names",
			write: func(ctx context.Context, s *Session) error {
				return s.Insert(ctx, Request{Caller: longLibrary, ID: "request-1"}, longLibraryKey, value)
			},
			want: cost(requestOverhead, len("request-1"), len(memberA), len(longLibrary.Library),
				len(longLibrary.Version)) + cost(keyOverhead, len(longLibraryKey), len(value)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)

			require.NoError(t, tt.write(context.Background(), f.session))

			assert.Equal(t, tt.want, used(f.session))
		})
	}
}

func TestAwaitingABarrierChargesItsName(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, f.session.AwaitBarrier(ctx, agentOf(memberA), barrierName), context.Canceled)

	assert.Equal(t, cost(barrierOverhead, len(barrierName)), used(f.session))
}

func TestARepeatedEntryIsChargedOnce(t *testing.T) {
	f := newFixture(t)
	ctx, r := context.Background(), requestOf(memberA, "")

	require.NoError(t, f.session.Insert(ctx, r, agentKey, []byte("value")))
	require.NoError(t, f.session.Insert(ctx, r, agentKey, []byte("value")))
	require.ErrorIs(t, f.session.Insert(ctx, r, agentKey, []byte("another")), ErrExists)
	require.NoError(t, f.session.PostBarrier(ctx, r, barrierName, ParticipantID(memberA)))
	require.NoError(t, f.session.PostBarrier(ctx, r, barrierName, ParticipantID(memberA)))

	assert.Equal(t, cost(keyOverhead, len(agentKey), len("value"))+
		cost(barrierOverhead, len(barrierName))+cost(participantOverhead, len(memberA)), used(f.session))
}

func TestAWriteFitsTheBudgetUpToItsLastByte(t *testing.T) {
	value := []byte("value")
	charge := cost(keyOverhead, len(agentKey), len(value))
	tests := []struct {
		name string
		room int64
		want error
	}{
		{name: "more room than the write needs", room: charge + 1},
		{name: "exactly the room the write needs", room: charge},
		{name: "one byte less than the write needs", room: charge - 1, want: ErrTooMany},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			fill(f.session, MaxSessionBytes-tt.room)

			err := f.session.Insert(context.Background(), requestOf(memberA, ""), agentKey, value)

			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				assert.Equal(t, MaxSessionBytes-tt.room, used(f.session), "a refused write charges nothing")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestEveryKindOfEntryIsRefusedOnceTheBudgetIsFull(t *testing.T) {
	tests := []struct {
		name  string
		write func(context.Context, *Session) error
	}{
		{name: "an agent's key", write: func(ctx context.Context, s *Session) error {
			return s.Insert(ctx, requestOf(memberA, ""), agentKey, nil)
		}},
		{name: "a relay's counter", write: func(ctx context.Context, s *Session) error {
			_, err := s.Increment(ctx, Request{Caller: relayOf(memberA)}, relayKey)
			return err
		}},
		{name: "a library's barrier", write: func(ctx context.Context, s *Session) error {
			return s.CreateBarrier(ctx, Request{Caller: ncclOf(memberA)}, ncclOther, 1)
		}},
		{name: "a recorded request", write: func(ctx context.Context, s *Session) error {
			return s.Unset(ctx, requestOf(memberA, "request-1"), agentKey)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			fill(f.session, MaxSessionBytes)

			require.ErrorIs(t, tt.write(context.Background(), f.session), ErrTooMany)
		})
	}
}

func TestAParticipantIsRefusedOnceTheBudgetIsFull(t *testing.T) {
	f := newFixture(t)
	ctx, r := context.Background(), Request{Caller: ncclOf(memberA)}
	require.NoError(t, f.session.CreateBarrier(ctx, r, ncclOther, 2))
	fill(f.session, MaxSessionBytes)

	require.ErrorIs(t, f.session.PostBarrier(ctx, r, ncclOther, "pid-1"), ErrTooMany)
}

func TestUnsettingOrShrinkingGivesBytesBack(t *testing.T) {
	f := newFixture(t)
	ctx, r := context.Background(), requestOf(memberA, "")
	large, small := make([]byte, 1000), make([]byte, 10)
	require.NoError(t, f.session.Insert(ctx, r, agentKey, large))
	require.Equal(t, cost(keyOverhead, len(agentKey), len(large)), used(f.session))

	require.NoError(t, f.session.CompareAndSwap(ctx, r, agentKey, large, small))
	assert.Equal(t, cost(keyOverhead, len(agentKey), len(small)), used(f.session))
	require.ErrorIs(t, f.session.CompareAndSwap(ctx, r, agentKey, large, large), ErrMismatch)
	assert.Equal(t, cost(keyOverhead, len(agentKey), len(small)), used(f.session), "a refused swap charges nothing")
	require.NoError(t, f.session.Unset(ctx, r, agentKey))
	require.NoError(t, f.session.Unset(ctx, r, agentKey))

	assert.Zero(t, used(f.session))
}

func TestASwapThatGrowsPastTheBudgetIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx, r := context.Background(), requestOf(memberA, "")
	require.NoError(t, f.session.Insert(ctx, r, agentKey, []byte("held")))
	fill(f.session, MaxSessionBytes)

	require.ErrorIs(t, f.session.CompareAndSwap(ctx, r, agentKey, []byte("held"), []byte("longer")), ErrTooMany)
	require.NoError(t, f.session.CompareAndSwap(ctx, r, agentKey, []byte("held"), []byte("he")),
		"a value that shrinks always fits")
}

func TestTwoSessionsNeverShareABudget(t *testing.T) {
	f := newManagerFixture(t)
	full := f.ensure(t, specOf(attemptUID, memberA))
	other := f.ensure(t, specOf(otherUID, memberA))
	fill(full, MaxSessionBytes)
	ctx, r := context.Background(), requestOf(memberA, "")

	require.ErrorIs(t, full.Insert(ctx, r, agentKey, nil), ErrTooMany)
	require.NoError(t, other.Insert(ctx, r, agentKey, nil))
}

func TestTheBudgetIsForgottenWhenTheSessionEnds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.session.Insert(ctx, requestOf(memberA, "request-1"), agentKey, []byte("value")))
	require.NoError(t, f.session.PostBarrier(ctx, requestOf(memberA, ""), barrierName, ParticipantID(memberA)))

	require.NoError(t, f.session.Fail(ctx, agentOf(memberA), failureReason))

	assert.Zero(t, used(f.session))
	f.session.ledger.usage.mu.Lock()
	defer f.session.ledger.usage.mu.Unlock()
	assert.Empty(t, f.session.ledger.usage.keys)
	assert.Empty(t, f.session.ledger.usage.barriers)
}

func TestAForgottenRecordGivesBackWhatItWasCharged(t *testing.T) {
	f := newManagerFixtureOn(t, &flakyStore{Store: memory.New(), failures: 1})
	s := f.ensure(t, specOf(attemptUID))
	r := Request{Caller: longLibrary, ID: "request-1"}

	require.ErrorIs(t, s.Insert(context.Background(), r, longLibraryKey, []byte("value")), errBackendDown)

	assert.Zero(t, used(s), "a failure with no identity leaves no record, and no charge")
}

// longLibrary is a library caller whose names leave room for little else in a
// key, so a record of its requests holds far more than the request's id.
var longLibrary = Caller{
	Member:  memberA,
	Role:    RoleLibrary,
	Library: strings.Repeat("l", 500),
	Version: strings.Repeat("v", 500),
}

// longLibraryKey is a key in longLibrary's namespace.
var longLibraryKey = libraryNamespace + longLibrary.Library + "/" + longLibrary.Version + "/k"
