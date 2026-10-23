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

// libraryOf is the request a member's NCCL Checkpoint compatibility client makes,
// with an identifier only when asked for.
func libraryOf(member MemberID, id RequestID) Request {
	return Request{Caller: ncclOf(member), ID: id}
}

// seedUsage changes what the session believes library callers hold, so a test
// can stand at a quota's edge without writing up to it.
func seedUsage(f *fixture, change func(*usage)) {
	u := &f.session.ledger.usage
	u.mu.Lock()
	defer u.mu.Unlock()
	change(u)
}

// seeded returns the nth of the names a test seeds a quota with.
func seeded(n int) string {
	return fmt.Sprintf("%sseeded-%d", ncclNamespace, n)
}

func TestLibraryKeysAreCapped(t *testing.T) {
	f := newFixture(t, memberA)
	seedUsage(f, func(u *usage) {
		for i := range MaxLibraryKeys - 1 {
			u.keys[seeded(i)] = 0
		}
	})
	ctx, r := context.Background(), libraryOf(memberA, "")

	require.NoError(t, f.session.Insert(ctx, r, ncclKey, []byte("last")), "the last key under the cap")
	require.NoError(t, f.session.Insert(ctx, r, ncclKey, []byte("last")), "an existing key at the cap")
	require.ErrorIs(t, f.session.Insert(ctx, r, ncclOther, []byte("over")), ErrTooMany)
	_, err := f.session.Increment(ctx, r, ncclOther)
	require.ErrorIs(t, err, ErrTooMany, "a counter is a key too")

	require.NoError(t, f.session.Unset(ctx, r, ncclKey))
	require.NoError(t, f.session.Insert(ctx, r, ncclOther, []byte("freed")), "unsetting a key frees its slot")
}

func TestLibraryBytesAreCapped(t *testing.T) {
	const room = 4
	tests := []struct {
		name  string
		apply func(context.Context, *Session, Request) error
		want  error
	}{
		{name: "a value that fills the room", apply: func(ctx context.Context, s *Session, r Request) error {
			return s.Insert(ctx, r, ncclKey, make([]byte, room))
		}},
		{name: "a value one byte over", want: ErrTooMany, apply: func(ctx context.Context, s *Session, r Request) error {
			return s.Insert(ctx, r, ncclKey, make([]byte, room+1))
		}},
		{name: "a swap that grows past the room", want: ErrTooMany,
			apply: func(ctx context.Context, s *Session, r Request) error {
				return s.CompareAndSwap(ctx, r, ncclOther, []byte("held"), make([]byte, len("held")+room+1))
			}},
		{name: "a swap that shrinks", apply: func(ctx context.Context, s *Session, r Request) error {
			return s.CompareAndSwap(ctx, r, ncclOther, []byte("held"), nil)
		}},
		{name: "a new counter over the room", want: ErrTooMany,
			apply: func(ctx context.Context, s *Session, r Request) error {
				_, err := s.Increment(ctx, r, ncclKey)
				return err
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memberA)
			ctx, r := context.Background(), libraryOf(memberA, "")
			require.NoError(t, f.session.Insert(ctx, r, ncclOther, []byte("held")))
			seedUsage(f, func(u *usage) { u.bytes = MaxLibraryBytes - room })

			err := tt.apply(ctx, f.session, r)

			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestLibraryUsageFollowsEveryChange(t *testing.T) {
	f := newFixture(t, memberA)
	ctx, r := context.Background(), libraryOf(memberA, "")

	require.NoError(t, f.session.Insert(ctx, r, ncclKey, []byte("four")))
	require.NoError(t, f.session.CompareAndSwap(ctx, r, ncclKey, []byte("four"), []byte("seven!!")))
	require.ErrorIs(t, f.session.CompareAndSwap(ctx, r, ncclKey, []byte("wrong"), make([]byte, 100)), ErrMismatch)
	_, err := f.session.Increment(ctx, r, ncclOther)
	require.NoError(t, err)
	_, err = f.session.Increment(ctx, r, ncclOther)
	require.NoError(t, err)
	require.NoError(t, f.session.Unset(ctx, r, ncclKey))
	require.NoError(t, f.session.Unset(ctx, r, ncclKey))

	seedUsage(f, func(u *usage) {
		assert.Equal(t, map[string]int{ncclOther: 8}, u.keys)
		assert.Equal(t, 8, u.bytes)
	})
}

func TestLibraryBarriersAreCapped(t *testing.T) {
	f := newFixture(t, memberA)
	seedUsage(f, func(u *usage) {
		for i := range MaxLibraryBarriers - 1 {
			u.barriers[seeded(i)] = map[string]struct{}{}
		}
	})
	ctx, r := context.Background(), libraryOf(memberA, "")

	require.NoError(t, f.session.CreateBarrier(ctx, r, ncclOther, 1), "the last barrier under the cap")
	require.NoError(t, f.session.PostBarrier(ctx, r, ncclOther, "pid-1"), "an existing barrier at the cap")
	require.ErrorIs(t, f.session.CreateBarrier(ctx, r, ncclKey, 1), ErrTooMany)
	require.ErrorIs(t, f.session.PostBarrier(ctx, r, ncclKey, "pid-1"), ErrTooMany)
	require.ErrorIs(t, f.session.AwaitBarrier(ctx, r.Caller, ncclKey), ErrTooMany,
		"waiting would bring a barrier into being too")
}

func TestLibraryParticipantsAreCappedPerBarrier(t *testing.T) {
	f := newFixture(t, memberA)
	ctx, r := context.Background(), libraryOf(memberA, "")
	require.NoError(t, f.session.CreateBarrier(ctx, r, ncclOther, MaxLibraryBarrierParticipants+1))
	seedUsage(f, func(u *usage) {
		for i := range MaxLibraryBarrierParticipants - 1 {
			u.barriers[ncclOther][seeded(i)] = struct{}{}
		}
		u.participants = MaxLibraryBarrierParticipants - 1
	})

	require.NoError(t, f.session.PostBarrier(ctx, r, ncclOther, "last"), "the last participant under the cap")
	require.NoError(t, f.session.PostBarrier(ctx, r, ncclOther, "last"), "a participant already counted")
	require.ErrorIs(t, f.session.PostBarrier(ctx, r, ncclOther, "one-more"), ErrTooMany)
	require.NoError(t, f.session.PostBarrier(ctx, r, ncclKey, "elsewhere"), "another barrier has its own room")
}

func TestLibraryParticipantsAreCappedPerSession(t *testing.T) {
	f := newFixture(t, memberA)
	seedUsage(f, func(u *usage) { u.participants = MaxLibraryParticipants - 1 })
	ctx, r := context.Background(), libraryOf(memberA, "")

	require.NoError(t, f.session.PostBarrier(ctx, r, ncclOther, "last"), "the last participant under the cap")
	require.ErrorIs(t, f.session.PostBarrier(ctx, r, ncclKey, "one-more"), ErrTooMany)
}

func TestLibraryRequestsAreCapped(t *testing.T) {
	f := newFixture(t, memberA)
	f.session.ledger.mu.Lock()
	f.session.ledger.libraryRequests = MaxLibraryRequests - 1
	f.session.ledger.mu.Unlock()
	ctx := context.Background()

	_, err := f.session.Increment(ctx, libraryOf(memberA, "last"), ncclKey)
	require.NoError(t, err, "the last request under the cap")
	replayed, err := f.session.Increment(ctx, libraryOf(memberA, "last"), ncclKey)
	require.NoError(t, err, "a recorded request still replays at the cap")
	assert.Equal(t, int64(1), replayed)
	_, err = f.session.Increment(ctx, libraryOf(memberA, "one-more"), ncclKey)
	require.ErrorIs(t, err, ErrTooMany)
	_, err = f.session.Increment(ctx, libraryOf(memberA, ""), ncclKey)
	require.NoError(t, err, "a request without an identifier records nothing")
}

func TestSnapshotsOwnCallersIgnoreTheLibraryQuotas(t *testing.T) {
	f := newFixture(t, memberA)
	seedUsage(f, func(u *usage) {
		for i := range MaxLibraryKeys {
			u.keys[seeded(i)] = 0
		}
		u.bytes = MaxLibraryBytes
		for i := range MaxLibraryBarriers {
			u.barriers[seeded(i)] = map[string]struct{}{}
		}
		u.participants = MaxLibraryParticipants
	})
	f.session.ledger.mu.Lock()
	f.session.ledger.libraryRequests = MaxLibraryRequests
	f.session.ledger.mu.Unlock()
	ctx := context.Background()
	require.ErrorIs(t, f.session.Insert(ctx, libraryOf(memberA, ""), ncclKey, []byte("flood")), ErrTooMany)

	require.NoError(t, f.session.Insert(ctx, requestOf(memberA, "r-1"), agentKey, []byte("handle")))
	_, err := f.session.Increment(ctx, requestOf(memberA, "r-2"), agentOther)
	require.NoError(t, err)
	require.NoError(t, f.session.CreateBarrier(ctx, requestOf(memberA, "r-3"), barrierName, 1))
	require.NoError(t, f.session.PostBarrier(ctx, requestOf(memberA, "r-4"), barrierName, ParticipantID(memberA)))
	require.NoError(t, f.session.AwaitBarrier(ctx, agentOf(memberA), barrierName))
	require.NoError(t, f.session.Insert(ctx, Request{Caller: relayOf(memberA), ID: "r-5"}, relayKey, []byte("round")))
}

func TestLibraryUsageIsForgottenWhenTheSessionEnds(t *testing.T) {
	f := newFixture(t, memberA)
	ctx := context.Background()
	require.NoError(t, f.session.Insert(ctx, libraryOf(memberA, "r-1"), ncclKey, []byte("handle")))
	require.NoError(t, f.session.PostBarrier(ctx, libraryOf(memberA, ""), ncclOther, "pid-1"))

	require.NoError(t, f.session.Fail(ctx, agentOf(memberA), failureReason))

	seedUsage(f, func(u *usage) {
		assert.Empty(t, u.keys)
		assert.Zero(t, u.bytes)
		assert.Empty(t, u.barriers)
		assert.Zero(t, u.participants)
	})
	f.session.ledger.mu.Lock()
	defer f.session.ledger.mu.Unlock()
	assert.Empty(t, f.session.ledger.requests)
	assert.Zero(t, f.session.ledger.libraryRequests)
}
