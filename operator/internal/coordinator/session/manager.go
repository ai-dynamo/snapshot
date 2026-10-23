// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
)

// Epoch identifies one Manager instance. It is drawn at random when the
// Manager is built and travels in every Welcome, so a member whose stream broke
// can tell a reconnect to the same coordinator from a restarted one.
type Epoch uint64

// Config carries a Manager's dependencies. Only Store is required.
type Config struct {
	// Store holds every attempt's keys and barriers.
	Store kv.Store

	// Clock is the source of time for deadlines, waits, and latencies. It
	// defaults to the real clock.
	Clock clock.WithDelayedExecution

	// Observer receives what sessions do. It defaults to reporting nothing.
	Observer Observer
}

// Manager holds the sessions of the set resources its reconciler follows,
// level-triggered: Ensure while a resource is unfinished, and Remove once it is
// deleted, or once it is final and its session has ended. Sessions are
// isolated from one another, so a key or a barrier of one is invisible to
// every other. A Manager is safe for concurrent use.
type Manager struct {
	epoch    Epoch
	store    kv.Store
	clock    clock.WithDelayedExecution
	observer Observer

	mu       sync.Mutex
	sessions map[types.UID]*Session
	// shutdown is the reason Shutdown failed every session with, and the one
	// every session ensured afterwards starts failed with.
	shutdown Reason
}

// NewManager returns a Manager keeping its sessions in cfg.Store.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("a manager needs a store: %w", ErrInvalid)
	}
	epoch, err := newEpoch()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		epoch:    epoch,
		store:    cfg.Store,
		clock:    cfg.Clock,
		observer: cfg.Observer,
		sessions: make(map[types.UID]*Session),
	}
	if m.clock == nil {
		m.clock = clock.RealClock{}
	}
	if m.observer == nil {
		m.observer = nopObserver{}
	}
	return m, nil
}

// Epoch returns the identifier of this Manager instance.
func (m *Manager) Epoch() Epoch { return m.epoch }

// Ensure returns the session of the attempt spec describes, creating it if it
// does not exist. Ensuring an attempt again with the same spec returns the same
// session, whatever order the spec lists the members in; a spec that differs
// reports ErrInvalid, since an attempt's membership and deadline never change.
//
// A session whose deadline has already passed, or one ensured after Shutdown,
// is created already failed, so a member that attaches learns why at once.
//
// The reconciler ensures an attempt only while its set resource is unfinished.
// It never ensures one whose resource is final, which would open a session for
// an attempt that is over, and so never one it has removed either, since a UID
// names one attempt. Nothing here enforces that; it is the reconciler's half
// of the rule Remove states.
func (m *Manager) Ensure(spec Spec) (*Session, error) {
	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("attempt %s: %w", spec.UID, err)
	}
	s, created, failed := m.adopt(spec)
	if !created {
		if !s.matches(spec) {
			return nil, fmt.Errorf("attempt %s was ensured with another membership or deadline: %w",
				spec.UID, ErrInvalid)
		}
		return s, nil
	}
	m.observer.SessionOpened(spec.UID)
	if failed != "" {
		m.observer.SessionFailed(spec.UID, failed)
	}
	return s, nil
}

// Session returns the session of an attempt, or ErrUnknownSession for one
// never ensured or already removed. A session that has ended is still returned
// until Remove, and reports how it ended to every caller.
func (m *Manager) Session(uid types.UID) (*Session, error) {
	s, ok := m.lookup(uid)
	if !ok {
		return nil, fmt.Errorf("attempt %s: %w", uid, ErrUnknownSession)
	}
	return s, nil
}

// Remove discards the session of an attempt its reconciler is done with. The
// rule is to remove a session once its set resource is deleted, or once the
// resource is final and the session has ended, as Session.Done reports.
//
// A final state alone is not enough. A PodSetRestore is final once every
// member is released, yet its session stays open until the members leave,
// because NCCL may still be rebuilding its communicators through it, and
// removing it then would abort that. The rule never leaves a session behind: a
// session whose agents never come back ends at its deadline.
//
// A session still open, which under the rule means its resource was deleted,
// is failed first with ReasonCancelled, so every member still waiting is told.
// Remove then waits until nothing is left running in the session, drops its
// space, and only then forgets it, so a failure to drop is reported and a
// retry finds the session again. Removing an attempt with no session does
// nothing.
func (m *Manager) Remove(ctx context.Context, uid types.UID) error {
	s, ok := m.lookup(uid)
	if !ok {
		return nil
	}
	s.finish(ctx, &AbortError{Reason: ReasonCancelled})
	if err := awaitDone(ctx, s); err != nil {
		return fmt.Errorf("remove attempt %s: %w", uid, err)
	}
	// The space is dropped even if ctx is done by now: the session has ended,
	// and nothing else would drop it.
	if err := m.store.Drop(context.WithoutCancel(ctx), uid); err != nil {
		return fmt.Errorf("drop attempt %s: %w", uid, err)
	}
	m.forget(s)
	return nil
}

// Fail fails the session of an attempt on the coordinator's own authority, with
// no member asking. The service calls it when a member's grace period expires,
// and when a reattach shows the coordinator restarted; the reconciler will call
// it for attempts a restarted coordinator can no longer serve. The reason must
// be a valid condition reason. Fail reports ErrUnknownSession for an attempt
// with no session, and how the session ended for one that already has, as
// Session.Fail does.
func (m *Manager) Fail(ctx context.Context, uid types.UID, reason Reason) error {
	if err := validateReason(reason); err != nil {
		return err
	}
	s, ok := m.lookup(uid)
	if !ok {
		return fmt.Errorf("attempt %s: %w", uid, ErrUnknownSession)
	}
	return s.abort(ctx, reason)
}

// Shutdown fails every open session with reason, for the coordinator's graceful
// termination, and makes every session ensured afterwards start failed with it
// too. The reason must be a valid condition reason; ReasonCoordinatorLost is the
// one members expect from a coordinator that goes away.
func (m *Manager) Shutdown(ctx context.Context, reason Reason) error {
	if err := validateReason(reason); err != nil {
		return err
	}
	m.mu.Lock()
	m.shutdown = reason
	sessions := slices.Collect(maps.Values(m.sessions))
	m.mu.Unlock()

	for _, s := range sessions {
		s.finish(ctx, &AbortError{Reason: reason})
	}
	return nil
}

// adopt returns the session for spec, creating and starting it if it does not
// exist. It reports whether it created the session, and the reason a session
// it created started failed with, if any. A session is started before it is
// published, so the deadline is armed under the Manager's mutex, which no clock
// callback takes.
func (m *Manager) adopt(spec Spec) (*Session, bool, Reason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[spec.UID]; ok {
		return s, false, ""
	}
	s := newSession(spec, m.epoch, m.store, m.clock, m.observer)
	failed := s.start(m.shutdown)
	m.sessions[spec.UID] = s
	return s, true, failed
}

// lookup returns the session of an attempt, if there is one.
func (m *Manager) lookup(uid types.UID) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[uid]
	return s, ok
}

// forget drops a removed session. It compares identity rather than UID, so a
// concurrent Remove that already forgot the session does nothing twice.
func (m *Manager) forget(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[s.uid] == s {
		delete(m.sessions, s.uid)
	}
}

// awaitDone waits until s is done or ctx is. When both are, the session wins:
// it is removed all the same, so reporting ctx's error would mislead.
func awaitDone(ctx context.Context, s *Session) error {
	select {
	case <-s.Done():
		return nil
	case <-ctx.Done():
	}
	select {
	case <-s.Done():
		return nil
	default:
		return ctx.Err()
	}
}

// newEpoch draws an identifier for one Manager instance.
func newEpoch() (Epoch, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, fmt.Errorf("draw the manager's epoch: %w", err)
	}
	return Epoch(binary.BigEndian.Uint64(buf[:])), nil
}
