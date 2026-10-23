// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
)

// Welcome is what a member's agent receives when it attaches.
type Welcome struct {
	// Epoch identifies the Manager instance holding the session.
	Epoch Epoch

	// Rank is the attaching member's rank.
	Rank int

	// Size is the number of members.
	Size int

	// Members is the whole membership, ordered by rank, so Members[i].Rank is
	// i. It is a copy the caller may keep.
	Members []Member

	// Deadline bounds the whole attempt.
	Deadline time.Time
}

// Session is one attempt of a set capture or restore. It is safe for concurrent
// use.
//
// A session that has ended, whether it closed, failed, or reached its deadline,
// stays reachable through its Manager until Remove, without its keys or its
// barriers, and reports how it ended to every caller. A member that arrives
// late therefore learns the outcome rather than joining a fresh session.
type Session struct {
	uid      types.UID
	epoch    Epoch
	members  []Member
	ranks    map[MemberID]int
	deadline time.Time

	store kv.Store
	// space is the attempt's space as every caller writes it: charged to the
	// session's budget.
	space kv.Space

	clock    clock.WithDelayedExecution
	observer Observer
	// timer is the deadline, armed by start; it is nil for a session that
	// started ended.
	timer clock.Timer

	// ctx is the session's lifetime. It is cancelled, with the terminal error
	// as its cause, when the session ends, and every call into the space runs
	// under a context joined to it, which is how an abort reaches a pending
	// wait. The session holds it because it belongs to no one call.
	ctx    context.Context
	cancel context.CancelCauseFunc

	// done is closed once the session has ended and every call into its space
	// has returned, which is when the reconciler may remove it.
	done chan struct{}

	// inflight counts the calls inside the space, so that ending the session
	// drops the space only once all of them have returned. A call is counted
	// only across its call into the space, never across a call to the clock:
	// the deadline ends the session from a clock callback, and a clock may run
	// that callback under a lock its other methods take.
	inflight sync.WaitGroup

	ledger ledger
}

// newSession builds a session from a validated spec. The Manager arms it with
// start before anyone else can reach it.
func newSession(spec Spec, epoch Epoch, store kv.Store, cl clock.WithDelayedExecution, observer Observer) *Session {
	members := spec.byRank()
	ranks := make(map[MemberID]int, len(members))
	for _, m := range members {
		ranks[m.ID] = m.Rank
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	s := &Session{
		uid:      spec.UID,
		epoch:    epoch,
		members:  members,
		ranks:    ranks,
		deadline: spec.Deadline,
		store:    store,
		clock:    cl,
		observer: observer,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	s.space = budgetSpace{space: store.Space(spec.UID), budget: &s.ledger.budget, usage: &s.ledger.usage}
	s.ledger.reset()
	return s
}

// start arms the session's deadline or, when the Manager has shut down or the
// deadline has already passed, ends the session at once and returns why. It
// runs before the session is published, so no caller ever finds it open when
// it should not be, and the space it never used is left for Remove to drop.
func (s *Session) start(shutdown Reason) Reason {
	reason := shutdown
	remaining := s.deadline.Sub(s.clock.Now())
	if reason == "" && remaining <= 0 {
		reason = ReasonDeadlineExceeded
	}
	if reason == "" {
		s.timer = s.clock.AfterFunc(remaining, s.expire)
		return ""
	}
	err := &AbortError{Reason: reason}
	s.ledger.end(err)
	s.cancel(err)
	close(s.done)
	return reason
}

// UID returns the attempt the session belongs to.
func (s *Session) UID() types.UID { return s.uid }

// Done returns a channel that is closed once the session has ended, whether it
// closed or failed, and every call still running in it has returned. That is
// when its reconciler may remove it, as Manager.Remove describes.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err returns nil while Done is open, and how the session ended once it is
// closed: ErrClosed, or an *AbortError naming the reason.
func (s *Session) Err() error {
	select {
	case <-s.done:
	default:
		return nil
	}
	s.ledger.mu.Lock()
	defer s.ledger.mu.Unlock()
	return s.ledger.ended
}

// Attach admits a member's agent and returns what it needs to take part. It
// records nothing, so an agent whose stream broke may attach again and receive
// the same welcome. Only an agent attaches; a caller that is not a member is
// refused.
func (s *Session) Attach(c Caller) (Welcome, error) {
	if err := s.admit(c, Caller.authorizeAttend); err != nil {
		return Welcome{}, err
	}
	return Welcome{
		Epoch:    s.epoch,
		Rank:     s.ranks[c.Member],
		Size:     len(s.members),
		Members:  slices.Clone(s.members),
		Deadline: s.deadline,
	}, nil
}

// Leave records that a member's agent is done, and closes the session once
// every member has left. Leaving twice is harmless. Leaving does not cut a
// member off: until the session closes, its relay and its workload processes
// keep reading and writing, which is what a library that restores after
// release needs.
func (s *Session) Leave(ctx context.Context, c Caller) error {
	allLeft, err := s.recordLeave(c)
	if err != nil {
		return err
	}
	if allLeft {
		s.finish(ctx, ErrClosed)
	}
	return nil
}

// Fail ends the session with a reason, which must be a valid condition reason.
// Every pending call, and every later one, reports an *AbortError carrying it.
// An agent or a relay may fail the session; failing one that has already ended
// reports how it ended instead.
func (s *Session) Fail(ctx context.Context, c Caller, reason Reason) error {
	if err := validateReason(reason); err != nil {
		return err
	}
	if err := s.admit(c, Caller.authorizeFail); err != nil {
		return err
	}
	return s.abort(ctx, reason)
}

// abort fails the session with a validated reason, or reports how it ended if
// it already has.
func (s *Session) abort(ctx context.Context, reason Reason) error {
	if s.finish(ctx, &AbortError{Reason: reason}) {
		return nil
	}
	s.ledger.mu.Lock()
	defer s.ledger.mu.Unlock()
	return s.ledger.ended
}

// matches reports whether a validated spec describes this session's attempt as
// it was ensured, whatever order it lists the members in.
func (s *Session) matches(spec Spec) bool {
	return spec.Deadline.Equal(s.deadline) && slices.Equal(spec.byRank(), s.members)
}

// recordLeave marks the member as departed and reports whether it was the last.
func (s *Session) recordLeave(c Caller) (bool, error) {
	s.ledger.mu.Lock()
	defer s.ledger.mu.Unlock()
	if err := s.checkLocked(c); err != nil {
		return false, err
	}
	if err := c.authorizeAttend(); err != nil {
		return false, err
	}
	s.ledger.left[c.Member] = struct{}{}
	return len(s.ledger.left) == len(s.members), nil
}

// admit reports whether the open session takes c for what rule allows its
// role.
func (s *Session) admit(c Caller, rule func(Caller) error) error {
	s.ledger.mu.Lock()
	defer s.ledger.mu.Unlock()
	if err := s.checkLocked(c); err != nil {
		return err
	}
	return rule(c)
}

// checkLocked reports how the session ended, or that c is not a member. Every
// operation goes through it, so a caller learns that the session ended before
// anything else. The caller holds the ledger's mutex.
func (s *Session) checkLocked(c Caller) error {
	if s.ledger.ended != nil {
		return s.ledger.ended
	}
	if _, ok := s.ranks[c.Member]; !ok {
		return fmt.Errorf("member %q: %w", c.Member, ErrNotMember)
	}
	return nil
}

// finish ends the session and stops its deadline, and reports whether this
// call ended it.
func (s *Session) finish(ctx context.Context, err error) bool {
	if !s.end(ctx, err) {
		return false
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	return true
}

// expire is the deadline's callback. It cannot stop its own timer, since a
// clock may run it under the lock Stop takes, and need not, since the timer
// has fired.
func (s *Session) expire() {
	s.end(context.Background(), &AbortError{Reason: ReasonDeadlineExceeded})
}

// end moves the session to its terminal state once, and reports whether this
// call did. It cancels the session's context with err, which wakes every call
// waiting in the space; waits for every call still in the space to return;
// forgets what only an open session needs; drops the space; reports the
// outcome; and only then closes Done, so a caller that sees Done closed knows
// nothing is left running in the space.
//
// Dropping here is eager, to free the space as soon as nobody needs it, and a
// failure is not reported to whoever ended the session: Manager.Remove drops
// the space again and reports to the reconciler, which owns the cleanup.
func (s *Session) end(ctx context.Context, err error) bool {
	if !s.ledger.end(err) {
		return false
	}
	s.cancel(err)
	s.inflight.Wait()
	s.ledger.reset()
	_ = s.store.Drop(ctx, s.uid)

	var aborted *AbortError
	if errors.As(err, &aborted) {
		s.observer.SessionFailed(s.uid, aborted.Reason)
	} else {
		s.observer.SessionClosed(s.uid)
	}
	close(s.done)
	return true
}

// enter admits one call into the space for r. It reports how the session ended
// or that the caller is not a member; it returns the outcome recorded for a
// retried request, and true; and otherwise it counts the call in flight, with
// a pending outcome to set when the request carries an identifier. The caller
// ends a counted call with s.inflight.Done.
func (s *Session) enter(r Request) (*outcome, bool, error) {
	s.ledger.mu.Lock()
	defer s.ledger.mu.Unlock()
	if err := s.checkLocked(r.Caller); err != nil {
		return nil, false, err
	}
	var pending *outcome
	if r.ID != "" {
		recorded, replay, err := s.ledger.recallLocked(r)
		if err != nil || replay {
			return recorded, replay, err
		}
		pending = recorded
	}
	s.inflight.Add(1)
	return pending, false, nil
}

// call runs f against the space as c sees it, under a context that is done when
// ctx is or when the session ends.
func (s *Session) call(ctx context.Context, c Caller, f func(context.Context, kv.Space) error) error {
	if _, _, err := s.enter(Request{Caller: c}); err != nil {
		return err
	}
	defer s.inflight.Done()
	joined, release := s.join(ctx)
	defer release()
	return f(joined, s.space)
}

// mutate runs one write for r. A retried request returns the outcome recorded
// for it rather than applying again. A write runs to completion once started,
// whatever the caller's context does, so the outcome a retry replays is the
// real one; only the session's end cuts it short. It reports how long the call
// took.
func (s *Session) mutate(ctx context.Context, r Request, op Operation,
	apply func(context.Context, kv.Space) (int64, error)) (int64, error) {
	defer s.observeOperation(r.Caller.Role, op, s.clock.Now())

	pending, replay, err := s.enter(r)
	if err != nil {
		return 0, err
	}
	if replay {
		value, err := pending.wait(ctx)
		return value, s.settle(ctx, err)
	}
	unbound := context.WithoutCancel(ctx)
	joined, release := s.join(unbound)
	value, err := apply(joined, s.space)
	release()
	s.inflight.Done()
	s.record(r, pending, value, err)
	return value, s.settle(unbound, err)
}

// record sets the outcome of a request that carries an identifier. The record
// keeps the error's identity and never its message, which names the key. An
// error with no identity, such as a backend failure, is not kept, so a retry
// runs the request again; a retry already waiting gets the error as it is.
func (s *Session) record(r Request, pending *outcome, value int64, err error) {
	if pending == nil {
		return
	}
	kept, ok := identityOf(err)
	if !ok {
		s.ledger.forget(r)
		kept = err
	}
	pending.set(value, kept)
}

// join returns a context that is done when ctx is or when the session ends,
// with the session's terminal error as its cause in that case, and the function
// that releases it.
func (s *Session) join(ctx context.Context) (context.Context, func()) {
	joined, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(s.ctx, func() { cancel(context.Cause(s.ctx)) })
	return joined, func() {
		stop()
		cancel(nil)
	}
}

// settle reports an error from the space as the caller should see it: how the
// session ended if it has, the caller's own cancellation if that is what
// stopped the call, and otherwise what the space said.
func (s *Session) settle(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case s.ctx.Err() != nil:
		return context.Cause(s.ctx)
	case ctx.Err() != nil:
		return fmt.Errorf("session %s: %w", s.uid, ctx.Err())
	default:
		return err
	}
}

// observeOperation reports one operation's latency, with no lock held.
func (s *Session) observeOperation(role Role, op Operation, start time.Time) {
	s.observer.OperationLatency(s.uid, role, op, s.clock.Since(start))
}
