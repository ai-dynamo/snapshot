// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestNewManagerNeedsAStore(t *testing.T) {
	_, err := NewManager(Config{})

	require.ErrorIs(t, err, ErrInvalid)
}

func TestEveryManagerHasItsOwnEpoch(t *testing.T) {
	first, second := newManagerFixture(t), newManagerFixture(t)

	assert.NotEqual(t, first.manager.Epoch(), second.manager.Epoch())
	assert.NotZero(t, first.manager.Epoch())
}

func TestEnsureReturnsOneSessionPerAttempt(t *testing.T) {
	f := newManagerFixture(t)
	spec := specOf(attemptUID)
	first := f.ensure(t, spec)
	reordered := spec
	reordered.Members = slices.Clone(spec.Members)
	slices.Reverse(reordered.Members)

	again, err := f.manager.Ensure(spec)
	require.NoError(t, err)
	listedOtherwise, err := f.manager.Ensure(reordered)
	require.NoError(t, err)

	assert.Same(t, first, again)
	assert.Same(t, first, listedOtherwise, "the order a spec lists its members in does not matter")
	assert.Equal(t, []types.UID{attemptUID}, f.observer.snapshot().opened, "a session is opened once")
}

func TestEnsureRefusesASpecThatChanged(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Spec)
	}{
		{name: "another deadline", change: func(s *Spec) { s.Deadline = s.Deadline.Add(time.Second) }},
		{name: "another member", change: func(s *Spec) { s.Members[1].ID = memberC }},
		{name: "swapped ranks", change: func(s *Spec) { s.Members[0].Rank, s.Members[1].Rank = 1, 0 }},
		{name: "one more member", change: func(s *Spec) { *s = specOf(attemptUID, memberA, memberB, memberC) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			changed := specOf(attemptUID)
			tt.change(&changed)

			_, err := f.manager.Ensure(changed)

			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestEnsureValidatesTheSpec(t *testing.T) {
	deadline := baseTime.Add(deadlineIn)
	tests := []struct {
		name string
		spec Spec
	}{
		{name: "no UID", spec: Spec{Deadline: deadline, Members: []Member{{ID: memberA}}}},
		{name: "no members", spec: Spec{UID: attemptUID, Deadline: deadline}},
		{name: "no deadline", spec: Spec{UID: attemptUID, Members: []Member{{ID: memberA}}}},
		{name: "a member with no ID", spec: Spec{UID: attemptUID, Deadline: deadline, Members: []Member{{}}}},
		{
			name: "the same member twice",
			spec: Spec{UID: attemptUID, Deadline: deadline, Members: []Member{{ID: memberA}, {ID: memberA, Rank: 1}}},
		},
		{
			name: "the same rank twice",
			spec: Spec{UID: attemptUID, Deadline: deadline, Members: []Member{{ID: memberA}, {ID: memberB}}},
		},
		{
			name: "a rank outside the set",
			spec: Spec{UID: attemptUID, Deadline: deadline, Members: []Member{{ID: memberA}, {ID: memberB, Rank: 2}}},
		},
		{
			name: "a negative rank",
			spec: Spec{UID: attemptUID, Deadline: deadline, Members: []Member{{ID: memberA, Rank: -1}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newManagerFixture(t)

			_, err := f.manager.Ensure(tt.spec)

			require.ErrorIs(t, err, ErrInvalid)
			assert.Empty(t, f.observer.snapshot().opened)
		})
	}
}

func TestConcurrentEnsuresShareOneSession(t *testing.T) {
	const callers = 16
	f := newManagerFixture(t)
	sessions := make(chan *Session, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			s, err := f.manager.Ensure(specOf(attemptUID))
			assert.NoError(t, err)
			sessions <- s
		})
	}
	wg.Wait()
	close(sessions)

	first := <-sessions
	for s := range sessions {
		assert.Same(t, first, s)
	}
	assert.Equal(t, []types.UID{attemptUID}, f.observer.snapshot().opened)
	assert.Equal(t, 1, f.clock.Waiters(), "one deadline is armed")
}

func TestEnsureWithAPastDeadlineStartsFailed(t *testing.T) {
	f := newManagerFixture(t)
	spec := specOf(attemptUID)
	spec.Deadline = baseTime.Add(-time.Nanosecond)

	s, err := f.manager.Ensure(spec)

	require.NoError(t, err)
	_, err = s.Attach(agentOf(memberA))
	assertAbortedWith(t, err, ReasonDeadlineExceeded)
	recorded := f.observer.snapshot()
	assert.Equal(t, []types.UID{attemptUID}, recorded.opened)
	assert.Equal(t, []Reason{ReasonDeadlineExceeded}, recorded.failed)
	assert.Zero(t, f.clock.Waiters(), "a session that starts failed arms no deadline")
}

func TestSessionFindsOnlyEnsuredAttempts(t *testing.T) {
	f := newFixture(t)

	found, err := f.manager.Session(attemptUID)
	require.NoError(t, err)
	assert.Same(t, f.session, found)
	_, err = f.manager.Session(otherUID)
	require.ErrorIs(t, err, ErrUnknownSession)

	require.NoError(t, f.manager.Remove(context.Background(), attemptUID))

	_, err = f.manager.Session(attemptUID)
	require.ErrorIs(t, err, ErrUnknownSession)
}

func TestRemovingADeletedAttemptFailsItsOpenSession(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.session.Insert(context.Background(), requestOf(memberA, ""), agentKey, []byte("handle")))
	pending := getAsync(t, f, agentOf(memberB), agentOther, 2*deadlineIn)

	require.NoError(t, f.manager.Remove(context.Background(), attemptUID))

	assertAbortedWith(t, await(t, pending), ReasonCancelled)
	_, err := f.session.Attach(agentOf(memberA))
	assertAbortedWith(t, err, ReasonCancelled)
	_, err = f.stored(attemptUID, agentKey)
	require.ErrorIs(t, err, ErrNotFound, "removing an attempt drops its space")
	assert.Zero(t, f.clock.Waiters(), "removing an attempt stops its deadline")
}

func TestRemoveKeepsHowASessionEnded(t *testing.T) {
	f := newFixture(t, memberA)
	require.NoError(t, f.session.Leave(context.Background(), agentOf(memberA)))

	require.NoError(t, f.manager.Remove(context.Background(), attemptUID))

	_, err := f.session.Attach(agentOf(memberA))
	require.ErrorIs(t, err, ErrClosed, "a session that already closed is not failed by its removal")
	assert.Equal(t, []types.UID{attemptUID}, f.observer.snapshot().closed)
	assert.Empty(t, f.observer.snapshot().failed)
}

func TestRemovingAnUnknownAttemptDoesNothing(t *testing.T) {
	f := newManagerFixture(t)

	require.NoError(t, f.manager.Remove(context.Background(), attemptUID))
}

func TestSessionsAreIsolatedByAttempt(t *testing.T) {
	f := newManagerFixture(t)
	first := f.ensure(t, specOf(attemptUID, memberA))
	second := f.ensure(t, specOf(otherUID, memberA))
	ctx, r := context.Background(), requestOf(memberA, "")

	require.NoError(t, first.Insert(ctx, r, agentKey, []byte("first")))
	require.NoError(t, second.Insert(ctx, r, agentKey, []byte("second")))

	firstValue, err := first.Get(ctx, agentOf(memberA), agentKey, 0)
	require.NoError(t, err)
	secondValue, err := second.Get(ctx, agentOf(memberA), agentKey, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), firstValue)
	assert.Equal(t, []byte("second"), secondValue)
}

func TestAFailedSessionDoesNotReachAnother(t *testing.T) {
	f := newManagerFixture(t)
	first := f.ensure(t, specOf(attemptUID, memberA))
	second := f.ensure(t, specOf(otherUID, memberA))
	ctx := context.Background()
	require.NoError(t, second.Insert(ctx, requestOf(memberA, ""), agentKey, []byte("second")))

	require.NoError(t, first.Fail(ctx, agentOf(memberA), failureReason))

	value, err := second.Get(ctx, agentOf(memberA), agentKey, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("second"), value)
}

func TestShutdownFailsEveryOpenSession(t *testing.T) {
	f := newManagerFixture(t)
	open := f.ensure(t, specOf(attemptUID, memberA))
	closed := f.ensure(t, specOf(otherUID, memberA))
	ctx := context.Background()
	require.NoError(t, closed.Leave(ctx, agentOf(memberA)))
	pending := barrierAsync(open, agentOf(memberA), barrierName)

	require.NoError(t, f.manager.Shutdown(ctx, ReasonCoordinatorLost))

	assertAbortedWith(t, await(t, pending), ReasonCoordinatorLost)
	_, err := closed.Attach(agentOf(memberA))
	require.ErrorIs(t, err, ErrClosed, "a session that already ended keeps its outcome")
	assert.Zero(t, f.clock.Waiters(), "shutting down stops every deadline")
}

func TestEnsureAfterShutdownStartsFailed(t *testing.T) {
	f := newManagerFixture(t)
	require.NoError(t, f.manager.Shutdown(context.Background(), ReasonCoordinatorLost))

	s, err := f.manager.Ensure(specOf(attemptUID))

	require.NoError(t, err)
	_, err = s.Attach(agentOf(memberA))
	assertAbortedWith(t, err, ReasonCoordinatorLost)
	assert.Zero(t, f.clock.Waiters())
}

func TestShutdownNeedsAReason(t *testing.T) {
	f := newFixture(t)

	require.ErrorIs(t, f.manager.Shutdown(context.Background(), ""), ErrInvalid)

	_, err := f.session.Attach(agentOf(memberA))
	require.NoError(t, err, "a refused shutdown leaves every session open")
}

func TestAManagerDefaultsItsClockAndObserver(t *testing.T) {
	manager, err := NewManager(Config{Store: memory.New()})
	require.NoError(t, err)
	spec := specOf(attemptUID, memberA)
	spec.Deadline = time.Now().Add(time.Hour)
	ctx := context.Background()

	s, err := manager.Ensure(spec)

	require.NoError(t, err)
	require.NoError(t, s.Insert(ctx, requestOf(memberA, ""), agentKey, []byte("handle")))
	require.NoError(t, s.Leave(ctx, agentOf(memberA)))
}

func TestManagerFailEndsAnOpenSessionWithoutACaller(t *testing.T) {
	f := newFixture(t)
	pending := getAsync(t, f, agentOf(memberA), agentKey, 2*deadlineIn)

	require.NoError(t, f.manager.Fail(context.Background(), attemptUID, ReasonMemberFailed))

	assertAbortedWith(t, await(t, pending), ReasonMemberFailed)
	assertAbortedWith(t, f.session.Err(), ReasonMemberFailed)
	assert.Zero(t, f.clock.Waiters(), "failing the session stops its deadline")
}

func TestManagerFailReportsWhatItCannotFail(t *testing.T) {
	tests := []struct {
		name  string
		end   func(*fixture) error
		check func(*testing.T, error)
	}{
		{
			name:  "an unknown attempt",
			end:   func(f *fixture) error { return f.manager.Remove(context.Background(), attemptUID) },
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, ErrUnknownSession) },
		},
		{
			name:  "a closed session",
			end:   func(f *fixture) error { return f.session.Leave(context.Background(), agentOf(memberA)) },
			check: func(t *testing.T, err error) { require.ErrorIs(t, err, ErrClosed) },
		},
		{
			name: "a failed session",
			end: func(f *fixture) error {
				return f.session.Fail(context.Background(), agentOf(memberA), failureReason)
			},
			check: func(t *testing.T, err error) { assertAbortedWith(t, err, failureReason) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, memberA)
			require.NoError(t, tt.end(f))

			tt.check(t, f.manager.Fail(context.Background(), attemptUID, ReasonCoordinatorLost))
		})
	}
}

func TestEveryReasonMustBeAConditionReason(t *testing.T) {
	tests := []struct {
		reason Reason
		valid  bool
	}{
		{reason: "MemberFailed", valid: true},
		{reason: "A", valid: true},
		{reason: "Lost:node_1,rack_2", valid: true},
		{reason: "Ends_", valid: true},
		{reason: Reason(strings.Repeat("R", 1024)), valid: true},
		{reason: ""},
		{reason: "1Lost"},
		{reason: "member failed"},
		{reason: "Lost-node"},
		{reason: "Ends,"},
		{reason: Reason(strings.Repeat("R", 1025))},
	}
	for _, tt := range tests {
		t.Run(string(tt.reason)[:min(len(tt.reason), 24)], func(t *testing.T) {
			ctx := context.Background()
			calls := map[string]func(*fixture) error{
				"Session.Fail":     func(f *fixture) error { return f.session.Fail(ctx, agentOf(memberA), tt.reason) },
				"Manager.Fail":     func(f *fixture) error { return f.manager.Fail(ctx, attemptUID, tt.reason) },
				"Manager.Shutdown": func(f *fixture) error { return f.manager.Shutdown(ctx, tt.reason) },
			}
			for name, call := range calls {
				f := newFixture(t)

				err := call(f)

				if tt.valid {
					require.NoError(t, err, name)
					continue
				}
				require.ErrorIs(t, err, ErrInvalid, name)
				require.NoError(t, f.session.Err(), "%s: an invalid reason leaves the session open", name)
			}
		})
	}
}

func TestRemoveWithAContextAlreadyDoneStillRemoves(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.session.Insert(context.Background(), requestOf(memberA, ""), agentKey, []byte("handle")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, f.manager.Remove(ctx, attemptUID), "a session done by now is removed, whatever ctx says")

	_, err := f.manager.Session(attemptUID)
	require.ErrorIs(t, err, ErrUnknownSession)
	_, err = f.stored(attemptUID, agentKey)
	require.ErrorIs(t, err, ErrNotFound, "the space is dropped even though ctx was done")
}
