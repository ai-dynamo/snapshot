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

	"github.com/ai-dynamo/snapshot/agent/pkg/session"
	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

	// Namespace returns the Store of one namespace. An unknown namespace
	// returns a Store whose every operation reports session.ErrInvalid.
	Namespace(ns session.Namespace) Store

	// Barrier records that this member has arrived at the barrier name, then
	// returns nil once every member in Meta has arrived. A barrier needs no
	// count and no creation; arriving again changes nothing; and once it has
	// released, a later arrival returns at once. The arrival is recorded even
	// if ctx is already done, so cancelling stops the wait, never the arrival.
	// The wait returns early, with how the session ended, if it ends first.
	// Only agents use barriers.
	Barrier(ctx context.Context, name string) error

	// Leave records that this member's agent is done. The session closes once
	// every member has left. Leaving twice changes nothing. A member that has
	// left keeps its connection until the session ends, because its relay and
	// its libraries may still use it, and the coordinator fails the session if
	// the connection is lost first.
	Leave(ctx context.Context) error

	// Fail ends the session with reason, a valid condition reason, if it has
	// not ended yet; every pending wait of every member then returns with it.
	// It returns nil if the session failed with this reason, whoever failed it
	// first, and how the session ended otherwise.
	Fail(ctx context.Context, reason string) error

	// Done is closed once the session has ended, or the coordinator was lost.
	// The session's connection closes then, since it has nothing left to
	// carry.
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

	// Set by join, and read-only afterwards.
	snapshot, library jetstream.KeyValue
	meta              session.Meta
	ranks             map[string]int

	// epoch is the epoch the client presents when it connects, zero until it
	// has read Meta.
	epoch atomic.Uint64

	inflight inflight

	connected   chan struct{}
	connect     sync.Once
	reconnected chan struct{}
	closing     chan struct{}
	done        chan struct{}
	end         sync.Once
	err         error

	mu sync.Mutex
	// verified is the reconnect count at which the client last found its
	// epoch in Meta; wake is closed and replaced when it advances.
	verified uint64
	wake     chan struct{}
	// closed is set by Close before it waits for the goroutines in wg.
	closed bool
	wg     sync.WaitGroup
}

// followEnd ends the session once the ended key appears.
func (s *client) followEnd(w watcher) {
	defer s.wg.Done()
	defer s.stop(w)
	for {
		select {
		case e, ok := <-w.Updates():
			if !ok {
				return
			}
			if e != nil && e.Operation() == jetstream.KeyValuePut {
				s.finish(session.EndedError(e.Value()))
				return
			}
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
		s.nc.Close()
	})
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
	if err := s.alive(); err != nil {
		return err
	}
	arrival := session.BarrierKey(name, s.member)
	if err := s.do(context.WithoutCancel(ctx), func(ctx context.Context) error {
		_, err := s.snapshot.Put(ctx, arrival, nil)
		return err
	}); err != nil {
		return err
	}
	prefix := session.BarrierKey(name, "")
	arrived := make(map[string]bool, len(s.ranks))
	if err := s.await(ctx, s.snapshot, []string{session.BarrierKey(name, "*")}, func(key string, _ []byte,
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
	if err := s.alive(); err != nil {
		return err
	}
	left := session.LeftKey(s.member)
	return s.do(ctx, func(ctx context.Context) error {
		_, err := s.snapshot.Put(ctx, left, nil)
		return err
	})
}

// Fail implements Session.
func (s *client) Fail(ctx context.Context, reason string) error {
	if err := session.ValidateReason(reason); err != nil {
		return err
	}
	err := s.alive()
	if err == nil {
		err = s.do(ctx, func(ctx context.Context) error {
			_, err := s.snapshot.Create(ctx, session.KeyEnded, []byte(reason))
			if !errors.Is(err, jetstream.ErrKeyExists) {
				return err
			}
			entry, err := s.snapshot.Get(ctx, session.KeyEnded)
			if err != nil {
				return err
			}
			return session.EndedError(entry.Value())
		})
	}
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
// fails for good, the session ends or is closed, or ctx is done.
func (s *client) do(ctx context.Context, op func(context.Context) error) error {
	for {
		if err := s.alive(); err != nil {
			return err
		}
		err := s.try(ctx, op)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ended := s.alive(); ended != nil {
			return ended
		}
		if !transient(err) {
			return mapError(err)
		}
		select {
		case <-time.After(retryWait):
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

// watcher is a watch, and the cancellation that ends it: a watch lives as long
// as the context it was started with.
type watcher struct {
	jetstream.KeyWatcher
	cancel context.CancelFunc
}

// watch starts watching keys of kv. Its creation is bounded like any operation,
// while the watch itself lives until stop.
func (s *client) watch(ctx context.Context, kv jetstream.KeyValue, keys []string) (watcher, error) {
	var w watcher
	err := s.do(ctx, func(attempt context.Context) error {
		life, cancel := context.WithCancel(context.Background())
		bounded := context.AfterFunc(attempt, cancel)
		// WatchFiltered rewrites the slice it is given.
		kw, err := kv.WatchFiltered(life, slices.Clone(keys))
		if !bounded() {
			// The attempt ended first, which cancelled the watch.
			err = context.Cause(attempt)
		}
		if err != nil {
			cancel()
			if kw != nil {
				drain(kw)
			}
			return err
		}
		w = watcher{KeyWatcher: kw, cancel: cancel}
		return nil
	})
	return w, err
}

// await watches keys of kv, and the ended key whenever kv is the Snapshot
// bucket, and passes visit every entry of keys, each key's current entry first
// and then every change, until visit reports true. present is false for a
// deleted key. Watching ended in the same watch orders the session's end among
// the entries, so an end recorded after the last entry visit needed does not
// hide it. Watching the library bucket relies on Done instead.
func (s *client) await(ctx context.Context, kv jetstream.KeyValue, keys []string,
	visit func(key string, value []byte, present bool) bool) error {
	watchesEnd := kv == s.snapshot
	if watchesEnd {
		keys = append(slices.Clone(keys), session.KeyEnded)
	}
	w, err := s.watch(ctx, kv, keys)
	if err != nil {
		return err
	}
	defer s.stop(w)
	for {
		select {
		case e, ok := <-w.Updates():
			switch {
			case !ok:
				if err := s.alive(); err != nil {
					return err
				}
				return net.ErrClosed
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

// stop stops a watch without holding up its caller: stopping deletes the
// watch's consumer, a round trip that waits out a broken connection. Close
// waits for it.
func (s *client) stop(w watcher) {
	w.cancel()
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if closed {
		drain(w)
		return
	}
	go func() {
		defer s.wg.Done()
		drain(w)
	}()
}

// drain waits until a cancelled watch has stopped. Its last entries may still
// be on their way, and its channel closes once its subscription has.
func drain(w jetstream.KeyWatcher) {
	for range w.Updates() {
	}
}
