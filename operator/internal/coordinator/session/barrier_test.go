// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostBarrierIsBoundToTheCallersMember(t *testing.T) {
	const relayBarrier = relayNamespace + "round"
	tests := []struct {
		name        string
		caller      Caller
		barrier     string
		participant ParticipantID
		want        error
	}{
		{name: "an agent as its member", caller: agentOf(memberA), barrier: barrierName, participant: "member-a"},
		{
			name:        "an agent as another member",
			caller:      agentOf(memberA),
			barrier:     barrierName,
			participant: ParticipantID(memberB),
			want:        ErrForbidden,
		},
		{
			name:        "an agent as a process",
			caller:      agentOf(memberA),
			barrier:     barrierName,
			participant: "pid-1",
			want:        ErrForbidden,
		},
		{name: "a relay as its member", caller: relayOf(memberA), barrier: relayBarrier, participant: "member-a"},
		{
			name:        "a relay as another member",
			caller:      relayOf(memberA),
			barrier:     relayBarrier,
			participant: ParticipantID(memberB),
			want:        ErrForbidden,
		},
		{name: "a library as a process", caller: ncclOf(memberA), barrier: ncclOther, participant: "pid-1"},
		{name: "no participant", caller: ncclOf(memberA), barrier: ncclOther, want: ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)

			err := f.session.PostBarrier(context.Background(), Request{Caller: tt.caller}, tt.barrier, tt.participant)

			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestBarrierReleasesOnceEveryMemberHasPosted(t *testing.T) {
	f := newFixture(t, memberA, memberB, memberC)
	ctx := context.Background()
	require.NoError(t, f.session.CreateBarrier(ctx, requestOf(memberA, ""), barrierName, 3))
	released := barrierAsync(f.session, agentOf(memberA), barrierName)

	for _, member := range []MemberID{memberA, memberA, memberB, memberC} {
		require.NoError(t, f.session.PostBarrier(ctx, requestOf(member, ""), barrierName, ParticipantID(member)))
	}

	require.NoError(t, await(t, released))
}

func TestBarrierTakesSeveralLibraryParticipantsFromOneMember(t *testing.T) {
	f := newFixture(t, memberA)
	ctx, library := context.Background(), ncclOf(memberA)
	r := Request{Caller: library}
	require.NoError(t, f.session.CreateBarrier(ctx, r, ncclOther, 3))
	released := barrierAsync(f.session, library, ncclOther)

	for pid := range 3 {
		require.NoError(t, f.session.PostBarrier(ctx, r, ncclOther, ParticipantID(fmt.Sprintf("pid-%d", pid))))
	}

	require.NoError(t, await(t, released))
}

func TestALibraryParticipantIsScopedByItsMember(t *testing.T) {
	f := newFixture(t, memberA, memberB)
	ctx := context.Background()
	first, second := Request{Caller: ncclOf(memberA)}, Request{Caller: ncclOf(memberB)}
	require.NoError(t, f.session.CreateBarrier(ctx, first, ncclOther, 2))

	require.NoError(t, f.session.PostBarrier(ctx, first, ncclOther, "rank-0"))
	require.NoError(t, f.session.PostBarrier(ctx, first, ncclOther, "rank-0"))
	assert.False(t, releasedInStore(f, ncclOther), "one member posting the same participant twice counts once")

	require.NoError(t, f.session.PostBarrier(ctx, second, ncclOther, "rank-0"))
	assert.True(t, releasedInStore(f, ncclOther), "another member posting the same participant counts again")
}

func TestAScopedParticipantNeverReadsAsAnotherPair(t *testing.T) {
	assert.NotEqual(t, ncclOf("member-a").scope("b/rank-0"), ncclOf("member-a/b").scope("rank-0"))
}

func TestAwaitBarrierReturnsWhenTheContextIsCancelled(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.session.CreateBarrier(context.Background(), requestOf(memberA, ""), barrierName, 2))
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { got <- f.session.AwaitBarrier(ctx, agentOf(memberA), barrierName) }()

	cancel()

	require.ErrorIs(t, await(t, got), context.Canceled)
}

func TestARetriedPostIsReplayedRatherThanReapplied(t *testing.T) {
	f := newFixture(t, memberA)
	ctx := context.Background()
	library := Request{Caller: ncclOf(memberA)}
	retried := Request{Caller: library.Caller, ID: "request-1"}
	require.NoError(t, f.session.CreateBarrier(ctx, library, ncclOther, 2))

	require.NoError(t, f.session.PostBarrier(ctx, retried, ncclOther, "pid-1"))
	require.NoError(t, f.session.PostBarrier(ctx, retried, ncclOther, "pid-2"))
	assert.False(t, releasedInStore(f, ncclOther), "the retry replays the first post rather than counting pid-2")

	require.NoError(t, f.session.PostBarrier(ctx, library, ncclOther, "pid-2"))
	assert.True(t, releasedInStore(f, ncclOther))
}

// releasedInStore reports whether a barrier has released, read straight from
// the store without blocking.
func releasedInStore(f *fixture, name string) bool {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	return f.store.Space(attemptUID).AwaitBarrier(done, name) == nil
}
