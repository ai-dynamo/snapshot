// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestAttachWelcomesAMember(t *testing.T) {
	f := newFixture(t)

	welcome, err := f.session.Attach(agentOf(memberB))

	require.NoError(t, err)
	assert.Equal(t, f.manager.Epoch(), welcome.Epoch)
	assert.Equal(t, 1, welcome.Rank)
	assert.Equal(t, 2, welcome.Size)
	assert.Equal(t, baseTime.Add(deadlineIn), welcome.Deadline)
	assert.Equal(t, []Member{{ID: memberA, Rank: 0}, {ID: memberB, Rank: 1}}, welcome.Members)
}

func TestAttachIsRepeatable(t *testing.T) {
	f := newFixture(t)

	first, err := f.session.Attach(agentOf(memberA))
	require.NoError(t, err)
	again, err := f.session.Attach(agentOf(memberA))

	require.NoError(t, err)
	assert.Equal(t, first, again)
}

func TestAttachHandsOutACopyOfTheMembership(t *testing.T) {
	f := newFixture(t)
	welcome, err := f.session.Attach(agentOf(memberA))
	require.NoError(t, err)

	welcome.Members[0].ID = "rewritten"
	again, err := f.session.Attach(agentOf(memberA))

	require.NoError(t, err)
	assert.Equal(t, memberA, again.Members[0].ID)
}

func TestAttachIsForAMembersAgentOnly(t *testing.T) {
	tests := []struct {
		name   string
		caller Caller
		want   error
	}{
		{name: "the agent of a member", caller: agentOf(memberA)},
		{name: "the agent of another attempt's member", caller: agentOf("member-z"), want: ErrNotMember},
		{name: "the relay of a member", caller: relayOf(memberA), want: ErrForbidden},
		{name: "a workload process", caller: ncclOf(memberA), want: ErrForbidden},
		{name: "a caller with no role", caller: Caller{Member: memberA}, want: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)

			_, err := f.session.Attach(tt.caller)

			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestOnlyAnAgentLeaves(t *testing.T) {
	f := newFixture(t, memberA)
	ctx := context.Background()

	require.ErrorIs(t, f.session.Leave(ctx, relayOf(memberA)), ErrForbidden)
	require.ErrorIs(t, f.session.Leave(ctx, ncclOf(memberA)), ErrForbidden)

	_, err := f.session.Attach(agentOf(memberA))
	require.NoError(t, err, "a relay leaving must not close the session out from under its agent")
}

func TestARelayMayFailTheSession(t *testing.T) {
	f := newFixture(t)

	require.NoError(t, f.session.Fail(context.Background(), relayOf(memberA), failureReason))

	_, err := f.session.Attach(agentOf(memberB))
	assertAbortedWith(t, err, failureReason)
}

func TestAMemberThatLeftKeepsReadingAndWriting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.session.Leave(ctx, agentOf(memberA)))

	require.NoError(t, f.session.Insert(ctx, Request{Caller: ncclOf(memberA)}, ncclKey, []byte("after release")))
	value, err := f.session.Get(ctx, ncclOf(memberA), ncclKey, 0)

	require.NoError(t, err)
	assert.Equal(t, []byte("after release"), value)
}

func TestSessionClosesWhenEveryMemberLeaves(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.session.Insert(ctx, requestOf(memberA, ""), agentKey, []byte("handle")))

	require.NoError(t, f.session.Leave(ctx, agentOf(memberA)))
	_, err := f.session.Get(ctx, agentOf(memberB), agentKey, 0)
	require.NoError(t, err, "one member leaving must not end the session")

	require.NoError(t, f.session.Leave(ctx, agentOf(memberB)))

	_, err = f.session.Get(ctx, agentOf(memberB), agentKey, 0)
	require.ErrorIs(t, err, ErrClosed)
	_, err = f.stored(attemptUID, agentKey)
	require.ErrorIs(t, err, ErrNotFound, "closing drops the session's keys")
	assert.Equal(t, []types.UID{attemptUID}, f.observer.snapshot().closed)
}

func TestLeaveIsIdempotentAndChecksMembership(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	require.NoError(t, f.session.Leave(ctx, agentOf(memberA)))
	require.NoError(t, f.session.Leave(ctx, agentOf(memberA)))
	require.ErrorIs(t, f.session.Leave(ctx, agentOf("member-z")), ErrNotMember)

	_, err := f.session.Attach(agentOf(memberB))
	require.NoError(t, err, "one member leaving twice must not close the session")
}

func TestALateCallerLearnsHowTheAttemptEnded(t *testing.T) {
	tests := []struct {
		name  string
		end   func(*fixture) error
		check func(*testing.T, error)
	}{
		{
			name:  "closed",
			end:   func(f *fixture) error { return f.session.Leave(context.Background(), agentOf(memberA)) },
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, ErrClosed) },
		},
		{
			name: "failed",
			end: func(f *fixture) error {
				return f.session.Fail(context.Background(), agentOf(memberA), failureReason)
			},
			check: func(t *testing.T, err error) { assertAbortedWith(t, err, failureReason) },
		},
		{
			name: "past its deadline",
			end: func(f *fixture) error {
				f.clock.Step(deadlineIn)
				return nil
			},
			check: func(t *testing.T, err error) { assertAbortedWith(t, err, ReasonDeadlineExceeded) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memberA)
			ctx := context.Background()
			require.NoError(t, tt.end(f))

			late, err := f.manager.Session(attemptUID)
			require.NoError(t, err, "an ended session stays until it is removed")
			assert.Same(t, f.session, late)
			_, err = late.Attach(agentOf(memberA))
			tt.check(t, err)
			tt.check(t, late.Insert(ctx, requestOf(memberA, "late"), agentKey, []byte("late")))
			_, err = late.Get(ctx, agentOf(memberA), agentKey, 0)
			tt.check(t, err)

			require.NoError(t, f.manager.Remove(ctx, attemptUID))

			_, err = f.manager.Session(attemptUID)
			require.ErrorIs(t, err, ErrUnknownSession)
		})
	}
}

func TestFailReachesEveryPendingCallThroughTheSessionContext(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.session.CreateBarrier(ctx, requestOf(memberA, ""), barrierName, 2))
	read := getAsync(t, f, agentOf(memberA), agentKey, deadlineIn)
	barrier := barrierAsync(f.session, agentOf(memberA), barrierName)

	require.NoError(t, f.session.Fail(ctx, agentOf(memberB), failureReason))

	assertAbortedWith(t, await(t, read), failureReason)
	assertAbortedWith(t, await(t, barrier), failureReason)
	assert.Equal(t, &AbortError{Reason: failureReason}, context.Cause(f.session.ctx),
		"the session's context carries the abort as its cause")
	assert.Equal(t, []Reason{failureReason}, f.observer.snapshot().failed)
}

func TestEveryLaterCallReportsTheReason(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.session.Fail(ctx, agentOf(memberA), failureReason))
	r := requestOf(memberA, "")

	calls := map[string]error{
		"insert":           f.session.Insert(ctx, r, agentKey, []byte("handle")),
		"unset":            f.session.Unset(ctx, r, agentKey),
		"compare and swap": f.session.CompareAndSwap(ctx, r, agentKey, nil, nil),
		"barrier create":   f.session.CreateBarrier(ctx, r, barrierName, 1),
		"barrier post":     f.session.PostBarrier(ctx, r, barrierName, ParticipantID(memberA)),
		"barrier wait":     f.session.AwaitBarrier(ctx, agentOf(memberA), barrierName),
		"leave":            f.session.Leave(ctx, agentOf(memberA)),
		"fail again":       f.session.Fail(ctx, agentOf(memberA), "AnotherReason"),
	}
	_, calls["increment"] = f.session.Increment(ctx, r, agentKey)
	_, calls["decrement"] = f.session.Decrement(ctx, r, agentKey)
	_, calls["get"] = f.session.Get(ctx, agentOf(memberA), agentKey, 0)
	_, calls["get with a wait"] = f.session.Get(ctx, agentOf(memberA), agentKey, deadlineIn)
	_, calls["attach"] = f.session.Attach(agentOf(memberA))

	for name, err := range calls {
		t.Run(name, func(t *testing.T) { assertAbortedWith(t, err, failureReason) })
	}
}

func TestFailNeedsAReasonAMemberAndARole(t *testing.T) {
	tests := []struct {
		name   string
		caller Caller
		reason Reason
		want   error
	}{
		{name: "no reason", caller: agentOf(memberA), want: ErrInvalid},
		{name: "another member", caller: agentOf("member-z"), reason: failureReason, want: ErrNotMember},
		{name: "a workload process", caller: ncclOf(memberA), reason: failureReason, want: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)

			require.ErrorIs(t, f.session.Fail(context.Background(), tt.caller, tt.reason), tt.want)

			_, err := f.session.Attach(agentOf(memberA))
			require.NoError(t, err, "a refused failure must leave the session open")
		})
	}
}

func TestTheDeadlineFailsAPendingWait(t *testing.T) {
	f := newFixture(t)
	// The wait outlasts the attempt, so stepping to the deadline fires the
	// session's timer and not this call's own.
	read := getAsync(t, f, agentOf(memberA), agentKey, 2*deadlineIn)

	f.clock.Step(deadlineIn)

	assertAbortedWith(t, await(t, read), ReasonDeadlineExceeded)
}

func TestTheDeadlineFailsTheSessionBeforeTheClockReturns(t *testing.T) {
	f := newFixture(t)

	f.clock.Step(deadlineIn)

	_, err := f.session.Attach(agentOf(memberA))
	assertAbortedWith(t, err, ReasonDeadlineExceeded)
	assert.Equal(t, []Reason{ReasonDeadlineExceeded}, f.observer.snapshot().failed)
}

func TestEndingTheSessionStopsTheDeadline(t *testing.T) {
	tests := []struct {
		name string
		end  func(*Session) error
	}{
		{name: "closing", end: func(s *Session) error { return s.Leave(context.Background(), agentOf(memberA)) }},
		{name: "failing", end: func(s *Session) error {
			return s.Fail(context.Background(), agentOf(memberA), failureReason)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memberA)
			require.Equal(t, 1, f.clock.Waiters())

			require.NoError(t, tt.end(f.session))

			assert.Zero(t, f.clock.Waiters())
			before := f.observer.snapshot().failed
			f.clock.Step(deadlineIn)
			assert.Equal(t, before, f.observer.snapshot().failed, "a stopped deadline reports nothing more")
		})
	}
}

func TestDoneAndErrReportHowTheSessionEnded(t *testing.T) {
	tests := []struct {
		name  string
		end   func(*fixture) error
		check func(*testing.T, error)
	}{
		{
			name:  "closed",
			end:   func(f *fixture) error { return f.session.Leave(context.Background(), agentOf(memberA)) },
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, ErrClosed) },
		},
		{
			name: "failed",
			end: func(f *fixture) error {
				return f.session.Fail(context.Background(), relayOf(memberA), failureReason)
			},
			check: func(t *testing.T, err error) { assertAbortedWith(t, err, failureReason) },
		},
		{
			name: "past its deadline",
			end: func(f *fixture) error {
				f.clock.Step(deadlineIn)
				return nil
			},
			check: func(t *testing.T, err error) { assertAbortedWith(t, err, ReasonDeadlineExceeded) },
		},
		{
			name:  "removed after its resource was deleted",
			end:   func(f *fixture) error { return f.manager.Remove(context.Background(), attemptUID) },
			check: func(t *testing.T, err error) { assertAbortedWith(t, err, ReasonCancelled) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memberA)
			assertOpen(t, f.session)

			require.NoError(t, tt.end(f))

			assertDone(t, f.session)
			tt.check(t, f.session.Err())
		})
	}
}

func TestASessionThatStartsFailedIsDoneAtOnce(t *testing.T) {
	f := newManagerFixture(t)
	spec := specOf(attemptUID)
	spec.Deadline = baseTime

	s, err := f.manager.Ensure(spec)

	require.NoError(t, err)
	assertDone(t, s)
	assertAbortedWith(t, s.Err(), ReasonDeadlineExceeded)
}

func TestAFinalAttemptIsRemovedOnlyOnceItsSessionHasEnded(t *testing.T) {
	// A restore is final once every member is released, but NCCL may still be
	// rebuilding its communicators through the session. A reconciler following
	// the rule leaves the session alone until it ends.
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.session.CreateBarrier(ctx, requestOf(memberA, ""), barrierName, 2))
	for _, member := range []MemberID{memberA, memberB} {
		require.NoError(t, f.session.PostBarrier(ctx, requestOf(member, ""), barrierName, ParticipantID(member)))
	}
	require.NoError(t, f.session.AwaitBarrier(ctx, agentOf(memberA), barrierName), "every member is released")
	assertOpen(t, f.session)

	nccl := Request{Caller: ncclOf(memberA)}
	require.NoError(t, f.session.Insert(ctx, nccl, ncclKey, []byte("rebuilt communicator")))
	value, err := f.session.Get(ctx, ncclOf(memberB), ncclKey, 0)
	require.NoError(t, err, "NCCL keeps using the session after release")
	assert.Equal(t, []byte("rebuilt communicator"), value)

	require.NoError(t, f.session.Leave(ctx, agentOf(memberA)))
	require.NoError(t, f.session.Leave(ctx, agentOf(memberB)))
	assertDone(t, f.session)
	require.ErrorIs(t, f.session.Err(), ErrClosed)

	require.NoError(t, f.manager.Remove(ctx, attemptUID))
	_, err = f.manager.Session(attemptUID)
	require.ErrorIs(t, err, ErrUnknownSession)
	assert.Empty(t, f.observer.snapshot().failed, "the attempt closed; nothing aborted it")
}

func TestEndingAndRemovingWaitUntilNoCallIsInsideTheSpace(t *testing.T) {
	store := &gatedStore{
		Store:     memory.New(),
		entered:   make(chan struct{}),
		cancelled: make(chan struct{}),
		release:   make(chan struct{}),
	}
	manager, err := NewManager(Config{Store: store, Clock: newSignalClock()})
	require.NoError(t, err)
	s, err := manager.Ensure(specOf(attemptUID))
	require.NoError(t, err)
	ctx := context.Background()
	inserted := make(chan error, 1)
	go func() { inserted <- s.Insert(ctx, requestOf(memberA, ""), agentKey, []byte("handle")) }()
	<-store.entered

	failed := make(chan error, 1)
	go func() { failed <- s.Fail(ctx, agentOf(memberB), failureReason) }()
	<-store.cancelled

	assertOpen(t, s)
	assert.Zero(t, store.drops.Load(), "the space is not dropped while a call is inside it")
	gaveUp, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, manager.Remove(gaveUp, attemptUID), context.Canceled,
		"Remove waits for the session to finish ending")
	_, err = manager.Session(attemptUID)
	require.NoError(t, err, "a Remove that gave up leaves the session for a retry")
	removed := make(chan error, 1)
	go func() { removed <- manager.Remove(ctx, attemptUID) }()

	close(store.release)

	require.NoError(t, await(t, inserted), "a write that started completes")
	require.NoError(t, await(t, failed))
	require.NoError(t, await(t, removed))
	assert.Zero(t, store.dropsSeenInside.Load(), "nothing dropped the space while the write was inside it")
	assertDone(t, s)
	assertAbortedWith(t, s.Err(), failureReason)
	assert.Equal(t, int64(2), store.drops.Load(), "the session drops its space, and Remove drops it again")
	_, err = manager.Session(attemptUID)
	require.ErrorIs(t, err, ErrUnknownSession)
}

func TestTheObserverSeesRolesAndNeverNames(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	library := Request{Caller: ncclOf(memberA)}
	require.NoError(t, f.session.Insert(ctx, requestOf(memberA, ""), agentKey, []byte("handle")))
	require.NoError(t, f.session.Insert(ctx, library, ncclKey, []byte("handle")))
	require.NoError(t, f.session.CreateBarrier(ctx, library, ncclOther, 1))
	require.NoError(t, f.session.PostBarrier(ctx, library, ncclOther, "pid-1"))
	require.NoError(t, f.session.AwaitBarrier(ctx, library.Caller, ncclOther))
	_, err := f.session.Get(ctx, relayOf(memberA), relayKey, 0)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.session.Get(ctx, agentOf(memberB), agentKey, deadlineIn)
	require.NoError(t, err)

	recorded := f.observer.snapshot()

	assert.Equal(t, []operation{
		{role: RoleAgent, op: OpInsert},
		{role: RoleLibrary, op: OpInsert},
		{role: RoleLibrary, op: OpCreateBarrier},
		{role: RoleLibrary, op: OpPostBarrier},
		{role: RoleRelay, op: OpGet},
	}, recorded.operations)
	assert.Equal(t, []Role{RoleAgent}, recorded.keyWaits)
	assert.Equal(t, []Role{RoleLibrary}, recorded.barrierWaits)
}

// gatedStore holds every Insert inside the space until release is closed, to
// show what ending and removing a session do while a call is still inside.
// entered is closed once an Insert is inside, and cancelled once the session
// has cancelled that Insert's context. dropsSeenInside is how many drops had
// happened when the Insert was about to leave the space.
type gatedStore struct {
	kv.Store
	entered         chan struct{}
	cancelled       chan struct{}
	release         chan struct{}
	drops           atomic.Int64
	dropsSeenInside atomic.Int64
}

func (g *gatedStore) Space(uid types.UID) kv.Space {
	return gatedSpace{Space: g.Store.Space(uid), store: g}
}

func (g *gatedStore) Drop(ctx context.Context, uid types.UID) error {
	g.drops.Add(1)
	return g.Store.Drop(ctx, uid)
}

type gatedSpace struct {
	kv.Space
	store *gatedStore
}

func (g gatedSpace) Insert(ctx context.Context, key string, value []byte) error {
	close(g.store.entered)
	<-ctx.Done()
	close(g.store.cancelled)
	<-g.store.release
	err := g.Space.Insert(ctx, key, value)
	g.store.dropsSeenInside.Store(g.store.drops.Load())
	return err
}

// assertOpen reports that a session has not ended.
func assertOpen(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.Done():
		t.Fatal("the session is done")
	default:
	}
	require.NoError(t, s.Err())
}

// assertDone reports that a session has ended and nothing runs in it.
func assertDone(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.Done():
	default:
		t.Fatal("the session is not done")
	}
	require.Error(t, s.Err())
}
