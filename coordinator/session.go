// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/internal/backoff"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/semaphore"
	"k8s.io/utils/clock"
)

// Session is one member's part in one attempt. Its operations are safe for
// concurrent use, and every one of them is safe to repeat: a client whose
// connection breaks sends again whatever it had not seen answered, which the
// session does itself until the operation succeeds, fails for good, the
// session ends, or the caller's context is done.
//
// Once the session has ended, every operation reports how, through Err.
type Session interface {
	// Meta returns what the member read when it connected: the membership,
	// the deadline, the epoch, and the value limits.
	Meta() session.Meta

	// Rank returns this member's rank, its position in Meta's membership.
	Rank() int

	// Snapshot returns the Store of the agents' keys.
	Snapshot() Store

	// CuInterpose returns the Store of the cuInterpose relays' keys, which the
	// agent hands its relay.
	CuInterpose() Store

	// Library returns the Store of workload libraries' keys, which the agent's
	// library endpoint serves.
	Library() Store

	// Barrier records that this member has arrived at the barrier name, then
	// returns nil once every member in Meta has arrived. A barrier needs no
	// count and no creation; arriving again changes nothing; and once it has
	// released, a later arrival returns at once. An arrival that was recorded
	// stays recorded: cancelling ctx stops the wait and never undoes the
	// arrival. A call whose ctx is already done returns its error and records
	// nothing. The wait returns early, with how the session ended, if it ends
	// first. A wait whose bucket holds its most consumers waits for one to
	// free. Only agents use barriers.
	Barrier(ctx context.Context, name string) error

	// Leave records that this member's agent is done. The session closes once
	// every member has left. Leaving twice changes nothing. A member that has
	// left keeps its connection until the session ends, because its relay and
	// its libraries may still use it, and the coordinator fails the session if
	// the connection is lost first. Leave returns nil if the session closed,
	// since it closes only once every member has left, this one included: the
	// last member's departure closes it, and the member may learn that before
	// the reply to its write. It reports any other ending.
	Leave(ctx context.Context) error

	// Fail ends the session with reason, a valid condition reason, if it has
	// not ended yet; every pending wait of every member then returns with it.
	// It returns nil if the session failed with this reason, whoever failed it
	// first, and how the session ended otherwise.
	Fail(ctx context.Context, reason string) error

	// Done is closed once the session has ended, or the coordinator was lost.
	// It is closed at the latest DeadlineSlack past Meta's deadline: a member
	// that has not learned how its session ended by then, because it cannot
	// reach the coordinator, ends the session itself, with
	// session.ReasonDeadlineExceeded. The session's connection closes then,
	// since it has nothing left to carry.
	Done() <-chan struct{}

	// Err returns nil until Done is closed, and then how the session ended:
	// session.ErrClosed, a *session.AbortError with the reason, or
	// session.ErrCoordinatorLost.
	Err() error

	// Close closes the connection, and does nothing else: it neither leaves
	// nor fails the session. Operations then report net.ErrClosed, unless the
	// session had ended.
	Close() error
}

// client implements Session over one NATS connection.
type client struct {
	attempt, member string
	log             logr.Logger
	nc              *nats.Conn
	clock           clock.Clock

	// Set by join, and read-only afterwards. joined is set last, as the
	// verifier starts.
	control bucket
	stores  map[session.Namespace]*store
	meta    session.Meta
	ranks   map[string]int
	joined  bool

	// epoch is the epoch the client presents when it connects, zero until it
	// has read Meta.
	epoch atomic.Uint64

	inflight inflight

	connected chan struct{}
	connect   sync.Once
	// verify holds a pending request to verify the session, so requests made
	// while one is pending coalesce.
	verify chan struct{}
	// budget holds, in bytes, what the coordinator buffers for the connection:
	// each value being read takes the largest its bucket holds, so no more are
	// in flight than the coordinator holds. Set by join.
	budget  *semaphore.Weighted
	closing chan struct{}
	// life ends once the session has ended or is closed, as done or closing
	// does, to end a wait that takes a context.
	life    context.Context
	endLife context.CancelFunc
	// connClosed is closed once the connection is, by Close or as the session
	// ends; nats.go then delivers nothing more to any watch.
	connClosed chan struct{}
	done       chan struct{}
	end        sync.Once
	err        error

	mu sync.Mutex
	// requested counts the requests to verify the session, and verified is the
	// target, which target returns, that the last verification to find the
	// session still there reached; wake is closed and replaced when verified
	// advances.
	requested, verified uint64
	wake                chan struct{}
	// closed is set by Close before it waits for the goroutines in wg.
	closed bool
	wg     sync.WaitGroup
}

// followEnd ends the session once the ended key appears, or the session is
// found gone, which await reports. A session never runs without its end being
// watched, so followEnd watches again after any other failure until the
// session ends or is closed. It waits attemptTimeout between tries, so a
// failure that persists, which only a bug should cause, is logged rarely.
func (s *client) followEnd() {
	defer s.wg.Done()
	for {
		err := s.await(context.Background(), s.control.kv, nil, func(string, []byte, bool) bool { return false })
		var abort *session.AbortError
		switch {
		case errors.As(err, &abort), errors.Is(err, session.ErrClosed):
			s.finish(err)
			return
		case errors.Is(err, net.ErrClosed):
			return
		}
		s.log.Error(err, "Watching the session's end failed; watching again")
		select {
		case <-time.After(attemptTimeout):
		case <-s.closing:
			return
		case <-s.done:
			return
		}
	}
}

// finish ends the session with err, once, and closes its connection, which has
// nothing left to carry.
func (s *client) finish(err error) {
	s.end.Do(func() {
		s.err = err
		close(s.done)
		s.endLife()
		s.nc.Close()
	})
}

// expire ends the session with session.ReasonDeadlineExceeded once Meta's
// deadline has passed by DeadlineSlack, unless the session ends or is closed
// first. It owns its timer, so nothing else touches it.
func (s *client) expire() {
	defer s.wg.Done()
	timer := s.clock.NewTimer(s.meta.Deadline.Sub(s.clock.Now()) + DeadlineSlack)
	defer timer.Stop()
	select {
	case <-timer.C():
		s.log.Info("The session's deadline passed before this member learned its end; ending it")
		s.finish(&session.AbortError{Reason: session.ReasonDeadlineExceeded})
	case <-s.done:
	case <-s.closing:
	}
}

// Done implements Session.
func (s *client) Done() <-chan struct{} { return s.done }

// Err implements Session.
func (s *client) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Close implements Session.
func (s *client) Close() error {
	s.mu.Lock()
	first := !s.closed
	s.closed = true
	s.mu.Unlock()
	if first {
		close(s.closing)
		s.endLife()
		s.nc.Close()
	}
	s.wg.Wait()
	return nil
}

// alive reports how the session ended, or net.ErrClosed once it is closed, and
// nil while it can still carry operations.
func (s *client) alive() error {
	select {
	case <-s.done:
		return s.Err()
	default:
	}
	select {
	case <-s.closing:
		return net.ErrClosed
	default:
		return nil
	}
}

// Barrier implements Session.
func (s *client) Barrier(ctx context.Context, name string) error {
	if err := session.ValidateName(name); err != nil {
		return fmt.Errorf("barrier: %w", err)
	}
	arrival := session.BarrierKey(name, s.member)
	if err := s.do(ctx, func(ctx context.Context) error {
		_, err := s.control.kv.Put(ctx, arrival, nil)
		return err
	}); err != nil {
		return err
	}
	prefix := session.BarrierKey(name, "")
	arrived := make(map[string]bool, len(s.ranks))
	if err := s.await(ctx, s.control.kv, []string{session.BarrierKey(name, "*")}, func(key string, _ []byte,
		present bool) bool {
		member := strings.TrimPrefix(key, prefix)
		if _, ok := s.ranks[member]; ok && present {
			arrived[member] = true
		} else {
			delete(arrived, member)
		}
		return len(arrived) == len(s.ranks)
	}); err != nil {
		return err
	}
	return s.settle(ctx)
}

// Leave implements Session.
func (s *client) Leave(ctx context.Context) error {
	left := session.LeftKey(s.member)
	err := s.do(ctx, func(ctx context.Context) error {
		_, err := s.control.kv.Put(ctx, left, nil)
		return err
	})
	if errors.Is(err, session.ErrClosed) {
		return nil
	}
	return err
}

// Fail implements Session.
func (s *client) Fail(ctx context.Context, reason string) error {
	if err := session.ValidateReason(reason); err != nil {
		return err
	}
	err := s.do(ctx, func(ctx context.Context) error {
		_, err := s.control.kv.Create(ctx, session.KeyEnded, []byte(reason))
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return err
		}
		entry, err := s.control.kv.Get(ctx, session.KeyEnded)
		if err != nil {
			return err
		}
		return session.EndedError(entry.Value())
	})
	if err == nil {
		s.finish(&session.AbortError{Reason: reason})
		return nil
	}
	if errors.Is(err, &session.AbortError{Reason: reason}) {
		return nil
	}
	return err
}

// do runs op, sending it again while it fails transiently, until it succeeds,
// fails for good, the session ends or is closed, or ctx is done, waiting longer
// before each try, as backoff.New spaces them. It never sends op once ctx is
// done. Each try is bounded by attemptTimeout and by a disconnect.
func (s *client) do(ctx context.Context, op func(context.Context) error) error {
	return s.retry(ctx, func(ctx context.Context) error { return s.try(ctx, op) })
}

// retry sends an operation through send, which tries it once, as do describes.
func (s *client) retry(ctx context.Context, send func(context.Context) error) error {
	waits := backoff.New()
	for {
		if err := s.alive(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err := send(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ended := s.alive(); ended != nil {
			return ended
		}
		switch {
		case missing(err) && !s.joined:
			// Nothing verifies the session yet: it was removed as the member
			// joined.
			return fmt.Errorf("attempt %s has no session: %w", s.attempt, session.ErrNotFound)
		case missing(err):
			// The bucket may be gone with the session.
			if err := s.confirm(ctx); err != nil {
				return err
			}
		case !transient(err):
			return mapError(err)
		}
		select {
		case <-time.After(waits.Step()):
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return s.Err()
		case <-s.closing:
			return net.ErrClosed
		}
	}
}

// try runs op once, bounded by attemptTimeout and by a disconnect.
func (s *client) try(ctx context.Context, op func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer s.inflight.add(cancel)()
	attempt, cancelAttempt := context.WithTimeout(ctx, attemptTimeout)
	defer cancelAttempt()
	err := op(attempt)
	if err != nil && errors.Is(context.Cause(ctx), errRetry) {
		return errRetry
	}
	return err
}

// read runs op, which reads one value, the way do runs an operation, with two
// differences. Each try first takes size bytes of the connection's budget, the
// most its reply holds, and keeps them until the reply has arrived or the
// connection has broken, even once ctx is done: until then, the reply may sit
// in what the coordinator buffers for the connection. And a try is bounded by
// ctx and by a disconnect only, never by attemptTimeout: a large value can take
// longer than a round trip to arrive, and a read sent again while its first
// reply is still buffered could take the connection past what the coordinator
// holds for it. So a read is never sent again on the same connection.
func (s *client) read(ctx context.Context, size int64, op func(context.Context) error) error {
	return s.retry(ctx, func(ctx context.Context) error { return s.readOnce(ctx, size, op) })
}

// readOnce tries a read once, as read describes. op runs on its own, bounded by
// a disconnect only, and holds its bytes until it returns, while readOnce
// returns op's error, or ctx's error or how the session ended if either comes
// first.
func (s *client) readOnce(ctx context.Context, size int64, op func(context.Context) error) error {
	if err := s.reserve(ctx, size); err != nil {
		return err
	}
	life, cancel := context.WithCancelCause(context.Background())
	untrack := s.inflight.add(cancel)
	release := func() {
		cancel(nil)
		untrack()
		s.budget.Release(size)
	}
	result := make(chan error, 1)
	if !s.spawn(func() {
		err := op(life)
		if err != nil && errors.Is(context.Cause(life), errRetry) {
			err = errRetry
		}
		release()
		result <- err
	}) {
		release()
		return net.ErrClosed
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.Err()
	case <-s.closing:
		return net.ErrClosed
	}
}

// reserve takes size bytes of the connection's budget, once the reads in
// flight leave room for them. It returns how the session ended, or ctx's
// error, if either comes first, and then takes nothing.
func (s *client) reserve(ctx context.Context, size int64) error {
	bounded, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.life, cancel)()
	err := s.budget.Acquire(bounded, size)
	if err == nil && bounded.Err() != nil {
		// Acquire may succeed although its context is done.
		s.budget.Release(size)
		err = bounded.Err()
	}
	if err != nil {
		if ended := s.alive(); ended != nil {
			return ended
		}
		return ctx.Err()
	}
	return nil
}

// watcher is a watch, and the cancellation that ends it: a watch lives as long
// as the context it was started with.
type watcher struct {
	jetstream.KeyWatcher
	cancel context.CancelFunc
}

// watch starts watching keys of kv, with opts. Its creation is bounded like any
// operation, while the watch itself lives until stop.
func (s *client) watch(ctx context.Context, kv jetstream.KeyValue, keys []string,
	opts ...jetstream.WatchOpt) (watcher, error) {
	var w watcher
	err := s.do(ctx, func(attempt context.Context) error {
		life, cancel := context.WithCancel(context.Background())
		bounded := context.AfterFunc(attempt, cancel)
		// WatchFiltered rewrites the slice it is given.
		kw, err := kv.WatchFiltered(life, slices.Clone(keys), opts...)
		if !bounded() {
			// The attempt ended first, which cancelled the watch.
			err = context.Cause(attempt)
		}
		if err != nil {
			if kw != nil {
				s.stop(watcher{KeyWatcher: kw, cancel: cancel})
			} else {
				cancel()
			}
			return err
		}
		w = watcher{KeyWatcher: kw, cancel: cancel}
		return nil
	})
	return w, err
}

// await watches keys of kv, and the ended key whenever kv is the control
// bucket, and passes visit every entry of keys, each key's current entry first
// and then every change, until visit reports true. present is false for a
// deleted key. Watching ended in the same watch orders the session's end among
// the entries, so an end recorded after the last entry visit needed does not
// hide it. Watching another bucket relies on Done instead. opts are the
// watch's, such as jetstream.MetaOnly, which delivers entries with no values.
func (s *client) await(ctx context.Context, kv jetstream.KeyValue, keys []string,
	visit func(key string, value []byte, present bool) bool, opts ...jetstream.WatchOpt) error {
	watchesEnd := kv == s.control.kv
	if watchesEnd {
		keys = append(slices.Clone(keys), session.KeyEnded)
	}
	w, err := s.watch(ctx, kv, keys, opts...)
	if err != nil {
		return err
	}
	defer func() { s.stop(w) }()
	for {
		select {
		case e, ok := <-w.Updates():
			switch {
			case !ok:
				// A watch that stops on its own may have lost its bucket with
				// the session. While the session is still there, a new watch
				// delivers the current entries again.
				if err := s.confirm(ctx); err != nil {
					return err
				}
				again, err := s.watch(ctx, kv, keys, opts...)
				if err != nil {
					return err
				}
				s.stop(w)
				w = again
			case e == nil:
				// The end of the current entries.
			case watchesEnd && e.Key() == session.KeyEnded:
				if e.Operation() == jetstream.KeyValuePut {
					return session.EndedError(e.Value())
				}
			case visit(e.Key(), e.Value(), e.Operation() == jetstream.KeyValuePut):
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return s.Err()
		case <-s.closing:
			return net.ErrClosed
		}
	}
}

// stop stops a watch without holding up its caller, and drains it so nats.go
// never blocks delivering to it. Close waits for the drains, which end by the
// time the connection has closed.
func (s *client) stop(w watcher) {
	w.cancel()
	if !s.spawn(func() { s.drain(w) }) {
		s.drain(w)
	}
}

// spawn runs f in a goroutine that Close waits for, and reports false, running
// nothing, once Close has begun.
func (s *client) spawn(f func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
	return true
}

// drain consumes a cancelled watch until its channel closes, which happens once
// its subscription has, or until the connection has closed. nats.go may never
// close the channel of a watch cancelled before WatchFiltered returned
// (nats-io/nats.go#2223), so the connection's close is what bounds the wait.
// nats.go delivers nothing after it, so emptying the channel frees the one
// delivery that may be blocked on it.
func (s *client) drain(w jetstream.KeyWatcher) {
	for {
		select {
		case _, ok := <-w.Updates():
			if !ok {
				return
			}
		case <-s.connClosed:
			for {
				select {
				case _, ok := <-w.Updates():
					if !ok {
						return
					}
				default:
					return
				}
			}
		}
	}
}
