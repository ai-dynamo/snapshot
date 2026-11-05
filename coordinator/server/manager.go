// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go/jetstream"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
)

// The errors a Server reports. They are the session package's, so a caller
// matches one set of sentinels on both sides.
var (
	// ErrInvalid reports a malformed argument, or a spec that contradicts the
	// one a session was ensured with.
	ErrInvalid = session.ErrInvalid

	// ErrNotFound reports an attempt the server holds no session for, and,
	// from an Authorizer, an attempt it cannot place yet.
	ErrNotFound = session.ErrNotFound
)

// errShutdown reports a call after Shutdown.
var errShutdown = errors.New("the coordinator has shut down")

// Spec is the fixed membership of one attempt, read from its set resource. A
// session never changes it.
type Spec struct {
	// UID is the set resource's UID, which identifies the attempt.
	UID types.UID

	// Members is the whole membership. It must name at least one member, with
	// distinct IDs that session.ValidateName accepts and ranks covering 0 to
	// len(Members)-1, in any order.
	Members []session.Member

	// Deadline bounds the whole attempt, counted from the resource's creation.
	Deadline time.Time
}

// validate reports whether the spec describes an attempt a session can run.
func (s Spec) validate() error {
	if err := session.ValidateName(string(s.UID)); err != nil {
		return fmt.Errorf("UID: %w", err)
	}
	if len(s.Members) == 0 {
		return fmt.Errorf("no members: %w", ErrInvalid)
	}
	if s.Deadline.IsZero() {
		return fmt.Errorf("no deadline: %w", ErrInvalid)
	}
	byID := make(map[string]struct{}, len(s.Members))
	byRank := make(map[int]struct{}, len(s.Members))
	for _, m := range s.Members {
		if err := session.ValidateName(m.ID); err != nil {
			return fmt.Errorf("member: %w", err)
		}
		if _, dup := byID[m.ID]; dup {
			return fmt.Errorf("member %q named twice: %w", m.ID, ErrInvalid)
		}
		if m.Rank < 0 || m.Rank >= len(s.Members) {
			return fmt.Errorf("member %q has rank %d outside 0..%d: %w", m.ID, m.Rank, len(s.Members)-1, ErrInvalid)
		}
		if _, dup := byRank[m.Rank]; dup {
			return fmt.Errorf("rank %d taken twice: %w", m.Rank, ErrInvalid)
		}
		byID[m.ID] = struct{}{}
		byRank[m.Rank] = struct{}{}
	}
	return nil
}

// byRank returns the membership of a validated spec ordered by rank, which is
// the one order two specs of the same attempt are compared in.
func (s Spec) byRank() []session.Member {
	members := make([]session.Member, len(s.Members))
	for _, m := range s.Members {
		members[m.Rank] = m
	}
	return members
}

// attempt is the session of one attempt.
type attempt struct {
	spec     Spec
	members  []session.Member
	epoch    uint64
	snapshot jetstream.KeyValue
	// life lasts until the session is removed or the coordinator shuts down,
	// which call cancel, or until follow has seen the session end. It bounds
	// following the session's keys and retrying its end, and no watch ends it.
	life   context.Context
	cancel context.CancelFunc

	// Guarded by Server.mu.
	deadline clock.Timer
	// conns holds the connections of each member, by client ID.
	conns map[string]map[uint64]struct{}
	// grace holds the grace timer of each member with no connection, and of a
	// member whose grace period ran out until the session's end is recorded.
	grace map[string]clock.Timer
	// ending is set once recordEnd has started recording the session's end.
	ending bool
	ended  bool
}

// has reports whether member is a member of the attempt.
func (a *attempt) has(member string) bool {
	return slices.ContainsFunc(a.members, func(m session.Member) bool { return m.ID == member })
}

// Ensure creates the session of the attempt spec describes, unless it exists:
// both buckets, with their largest values and the library's key limit, and
// meta, with a new random epoch. It arms the deadline, and starts following the
// members' departures and the session's end. Ensuring an attempt again with the same
// spec, whatever order it lists the members in, does nothing; a spec that
// differs reports ErrInvalid, since an attempt's membership and deadline never
// change. A session whose deadline has already passed is created already
// failed with session.ReasonDeadlineExceeded, so a member that connects learns
// why at once.
//
// The host ensures an attempt only while its set resource is unfinished.
// It never ensures one whose resource is final, which would open a session for
// an attempt that is over, and so never one it has removed either, since a UID
// names one attempt. Nothing here enforces that; it is the host's half of
// the rule Remove states.
func (s *Server) Ensure(ctx context.Context, spec Spec) error {
	if err := spec.validate(); err != nil {
		return fmt.Errorf("attempt %s: %w", spec.UID, err)
	}
	s.control.Lock()
	defer s.control.Unlock()
	if s.isShutdown() {
		return errShutdown
	}
	if a := s.lookup(spec.UID); a != nil {
		if !slices.Equal(a.members, spec.byRank()) || !a.spec.Deadline.Equal(spec.Deadline) {
			return fmt.Errorf("attempt %s was ensured with another membership or deadline: %w", spec.UID, ErrInvalid)
		}
		return nil
	}
	a, err := s.create(ctx, spec)
	if err != nil {
		return fmt.Errorf("create the session of attempt %s: %w", spec.UID, err)
	}
	s.mu.Lock()
	s.attempts[spec.UID] = a
	s.mu.Unlock()
	s.cfg.Observer.SessionOpened(spec.UID)
	s.log.Info("Session opened", "attempt", spec.UID, "members", len(a.members))

	remaining := spec.Deadline.Sub(s.cfg.Clock.Now())
	if remaining <= 0 {
		err := s.endWith(ctx, a, []byte(session.ReasonDeadlineExceeded))
		if err == nil || ended(err) {
			return nil
		}
		// The deadline timer, due at once, tries again.
		s.log.Error(err, "Failing a session past its deadline failed", "attempt", spec.UID)
		remaining = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !a.ended {
		var timer clock.Timer
		timer = s.cfg.Clock.AfterFunc(remaining, func() {
			s.recordEnd(a, []byte(session.ReasonDeadlineExceeded), func() bool { return a.deadline == timer })
		})
		a.deadline = timer
	}
	return nil
}

// create creates an attempt's buckets and meta, and starts following them. On
// any failure it deletes whatever buckets it created, even once ctx is done, so
// it leaves nothing behind and a retried Ensure starts afresh.
func (s *Server) create(ctx context.Context, spec Spec) (a *attempt, err error) {
	epoch, err := newEpoch()
	if err != nil {
		return nil, err
	}
	a = &attempt{
		spec:    spec,
		members: spec.byRank(),
		epoch:   epoch,
		conns:   make(map[string]map[uint64]struct{}),
		grace:   make(map[string]clock.Timer),
	}
	life, cancel := context.WithCancel(context.Background())
	a.life, a.cancel = life, cancel
	meta, err := json.Marshal(session.Meta{
		Members:         a.members,
		Deadline:        spec.Deadline,
		Epoch:           epoch,
		MaxValue:        s.cfg.MaxValue,
		MaxLibraryValue: s.cfg.MaxLibraryValue,
	})
	if err != nil {
		return nil, fmt.Errorf("encode meta: %w", err)
	}
	uid := string(spec.UID)
	defer func() {
		if err == nil {
			return
		}
		cancel()
		if err := s.deleteBuckets(context.WithoutCancel(ctx), spec.UID); err != nil {
			s.log.Error(err, "Deleting the buckets of a session that failed to open failed", "attempt", uid)
		}
	}()
	members := len(spec.Members)
	if a.snapshot, err = s.createBucket(ctx, session.SnapshotBucket(uid), 0, s.cfg.MaxValue,
		members*session.MaxConsumersPerMember+coordinatorConsumers); err != nil {
		return nil, err
	}
	if _, err = s.createBucket(ctx, session.LibraryBucket(uid), s.cfg.MaxLibraryKeys, s.cfg.MaxLibraryValue,
		members*session.MaxLibraryConsumersPerMember); err != nil {
		return nil, err
	}
	if _, err = a.snapshot.Put(ctx, session.KeyMeta, meta); err != nil {
		return nil, fmt.Errorf("write meta: %w", err)
	}
	if err = s.follow(ctx, a); err != nil {
		return nil, err
	}
	return a, nil
}

// coordinatorConsumers is how many consumers the coordinator holds on each
// Snapshot bucket: the watch of the departures and the end that follow starts.
// Its connection is in-process and never reconnects.
const coordinatorConsumers = 1

// createBucket creates one in-memory bucket, bounded by its largest value, by
// how many consumers it may have, and, unless maxKeys is zero, by its number of
// keys, none of which reserves memory. JetStream's key-value configuration has
// neither a consumer nor a key limit, so both are set on the bucket's stream,
// before any member can reach the bucket.
func (s *Server) createBucket(ctx context.Context, bucket string, maxKeys, maxValue, maxConsumers int) (
	jetstream.KeyValue, error) {
	kv, err := s.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       bucket,
		Storage:      jetstream.MemoryStorage,
		Replicas:     s.cfg.Replicas,
		MaxValueSize: int32(maxValue + valueHeadroom),
	})
	if err != nil {
		return nil, fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	stream, err := s.js.Stream(ctx, "KV_"+bucket)
	if err != nil {
		return nil, fmt.Errorf("bound bucket %s: %w", bucket, err)
	}
	cfg := stream.CachedInfo().Config
	cfg.MaxConsumers = maxConsumers
	if maxKeys != 0 {
		cfg.MaxMsgs = int64(maxKeys)
	}
	if _, err := s.js.UpdateStream(ctx, cfg); err != nil {
		return nil, fmt.Errorf("bound bucket %s: %w", bucket, err)
	}
	return kv, nil
}

// deleteBuckets deletes both of an attempt's buckets, whether or not they
// exist.
func (s *Server) deleteBuckets(ctx context.Context, uid types.UID) error {
	var errs []error
	for _, bucket := range []string{session.SnapshotBucket(string(uid)), session.LibraryBucket(string(uid))} {
		if err := s.js.DeleteKeyValue(ctx, bucket); err != nil && !errors.Is(err, jetstream.ErrBucketNotFound) &&
			!errors.Is(err, jetstream.ErrStreamNotFound) {
			errs = append(errs, fmt.Errorf("delete bucket %s: %w", bucket, err))
		}
	}
	return errors.Join(errs...)
}

// follow watches an attempt's departures and its end for as long as the
// session's life lasts, while ctx bounds only starting the first watch. Once
// every member has left, it closes the session; once the session has ended,
// however it ended, it records the outcome and ends the session's life. A
// watch can stop on its own: nats.go stops one whose consumer it cannot
// recreate, such as at the bucket's consumer limit. follow then watches again,
// so the session's life never depends on one watch.
func (s *Server) follow(ctx context.Context, a *attempt) error {
	w, stop, err := s.watch(ctx, a)
	if err != nil {
		return err
	}
	// Ensure holds Server.control, so Shutdown is not waiting for wg yet.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		left := make(map[string]bool, len(a.members))
		for w != nil {
			if s.track(a, w, left) {
				a.cancel()
			}
			stop()
			w, stop = s.rewatch(a)
		}
	}()
	return nil
}

// track passes the departures and the end a watch delivers to the session,
// until the session has ended, which it reports, or the watch stops. left
// holds the members that have left, across watches, so a watch that delivers
// the departures again closes the session once only.
func (s *Server) track(a *attempt, w jetstream.KeyWatcher, left map[string]bool) bool {
	leftPrefix := session.LeftKey("")
	for e := range w.Updates() {
		if e == nil || e.Operation() != jetstream.KeyValuePut {
			continue
		}
		if e.Key() == session.KeyEnded {
			s.end(a, e.Value())
			return true
		}
		member := strings.TrimPrefix(e.Key(), leftPrefix)
		if !a.has(member) || left[member] {
			continue
		}
		left[member] = true
		if len(left) == len(a.members) {
			s.recordEnd(a, nil, always)
		}
	}
	return false
}

// watch starts watching a session's departures and its end. The watch lasts
// until stop, or until the session's life ends, while ctx bounds only its
// creation. stop ends the watch and drains it.
func (s *Server) watch(ctx context.Context, a *attempt) (jetstream.KeyWatcher, func(), error) {
	life, cancel := context.WithCancel(a.life)
	bounded := context.AfterFunc(ctx, cancel)
	w, err := a.snapshot.WatchFiltered(life, []string{session.LeftKey("*"), session.KeyEnded})
	if !bounded() {
		err = ctx.Err()
	}
	if err != nil {
		cancel()
		if w != nil {
			// The watch was cancelled as it was created, so nats.go may never
			// close its channel (nats-io/nats.go#2223). Only the connection's
			// close, in Shutdown, then ends drain, and Shutdown waits for
			// control, which Ensure holds, so the watch drains on its own
			// instead of holding up its caller. Either Ensure, holding
			// control, or follow, counted in wg, calls watch, so Shutdown is
			// not waiting for wg yet.
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.drain(w)
			}()
		}
		return nil, nil, fmt.Errorf("watch the session: %w", err)
	}
	return w, func() {
		cancel()
		s.drain(w)
	}, nil
}

// rewatch watches a session again once its watch has stopped, waiting from
// firstEndRetry up to maxEndRetry before each try, until a watch starts. It
// returns a nil watch once the session's life has ended, or the coordinator's
// connection has closed, which a coordinator that stops without shutting down
// does, and which leaves nothing to watch with.
func (s *Server) rewatch(a *attempt) (jetstream.KeyWatcher, func()) {
	select {
	case <-a.life.Done():
		return nil, nil
	case <-s.connClosed:
		return nil, nil
	default:
	}
	s.log.Info("The session's watch stopped; watching again", "attempt", a.spec.UID)
	for wait := firstEndRetry; ; wait = min(2*wait, maxEndRetry) {
		select {
		case <-time.After(wait):
		case <-a.life.Done():
			return nil, nil
		case <-s.connClosed:
			return nil, nil
		}
		ctx, cancel := context.WithTimeout(a.life, tryTimeout)
		w, stop, err := s.watch(ctx, a)
		cancel()
		switch {
		case err == nil:
			return w, stop
		case a.life.Err() != nil:
			return nil, nil
		case wait == firstEndRetry:
			s.log.Error(err, "Watching the session again failed; retrying", "attempt", a.spec.UID)
		}
	}
}

// drain consumes a cancelled watch until its channel closes, which happens once
// its subscription has, or until the coordinator's connection has closed.
// nats.go may never close the channel of a watch cancelled before WatchFiltered
// returned (nats-io/nats.go#2223), so the connection's close, which Shutdown
// does before it waits for what the server started, is what bounds the wait.
// nats.go delivers nothing after it, so emptying the channel frees the one
// delivery that may be blocked on it.
func (s *Server) drain(w jetstream.KeyWatcher) {
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

// Remove discards the session of an attempt the host is done with: it
// fails the session first with session.ReasonCancelled if it is still open,
// recording the end when the memory cap allows, then deletes both buckets and
// forgets the session. A member that has not received the end by then learns
// from finding its session gone, with session.ErrCoordinatorLost. The rule is
// to remove a session once its set resource is deleted, or once the resource
// is final and the session has ended.
//
// A final state alone is not enough. A PodSetRestore is final once every
// member is released, yet its session stays open until the members leave,
// because NCCL may still be rebuilding its communicators through it, and
// removing it then would abort that. The rule never leaves a session behind: a
// session whose agents never come back ends at its deadline.
//
// A session that cannot be failed first, such as when the memory cap leaves no
// room for the ended key, is removed all the same, since removing it frees what
// the cap needs. A session whose buckets cannot be deleted is kept, so a retry
// finds it again. Removing an attempt with no session does nothing.
func (s *Server) Remove(ctx context.Context, uid types.UID) error {
	s.control.Lock()
	defer s.control.Unlock()
	if s.isShutdown() {
		return errShutdown
	}
	a := s.lookup(uid)
	if a == nil {
		return nil
	}
	if err := s.endWith(ctx, a, []byte(session.ReasonCancelled)); err != nil && !ended(err) {
		s.log.Error(err, "Cancelling a session before removing it failed", "attempt", uid)
	}
	a.cancel()
	if err := s.deleteBuckets(ctx, uid); err != nil {
		return fmt.Errorf("remove attempt %s: %w", uid, err)
	}
	s.discard(a)
	s.mu.Lock()
	delete(s.attempts, uid)
	s.mu.Unlock()
	s.log.Info("Session removed", "attempt", uid)
	return nil
}

// Fail fails the session of an attempt on the coordinator's own authority,
// with no member asking, as a member's Fail does: the reason must be a valid
// condition reason, the first reason wins, and Fail returns nil if the session
// failed with this reason, and how it ended otherwise. It reports ErrNotFound
// for an attempt with no session.
func (s *Server) Fail(ctx context.Context, uid types.UID, reason string) error {
	if err := session.ValidateReason(reason); err != nil {
		return err
	}
	a := s.lookup(uid)
	if a == nil {
		return fmt.Errorf("attempt %s: %w", uid, ErrNotFound)
	}
	err := s.endWith(ctx, a, []byte(reason))
	if errors.Is(err, &session.AbortError{Reason: reason}) {
		return nil
	}
	return err
}

// tryTimeout bounds each try to record a session's end, and each try to watch
// a session again.
const tryTimeout = 5 * time.Second

// The wait between tries to record a session's end, or to watch a session
// again: it starts at firstEndRetry and doubles up to maxEndRetry.
const (
	firstEndRetry = 100 * time.Millisecond
	maxEndRetry   = time.Second
)

// always arms recordEnd whatever calls it: closing a session every member has
// left.
func always() bool { return true }

// recordEnd ends a session with value, which becomes its ended key: a reason,
// such as when the deadline or a member's grace period runs out, or nothing,
// to close the session once every member has left. A timer that was stopped
// or replaced may fire all the same, so armed, which recordEnd calls under
// Server.mu, reports whether the timer that fired is still the one armed. A
// session that has ended is left alone, and so is every session once Shutdown
// has begun, which records its own end and waits for an end being recorded.
//
// Recording the end can fail, such as at the memory cap, which no write is
// exempt from and nothing reserves room under. A session that has to end must
// not stay open once memory frees, so recordEnd keeps trying in the
// background, waiting from firstEndRetry up to maxEndRetry, until the end is
// recorded, by recordEnd or anything else, or the session's life ends because
// it is removed or the coordinator shuts down. It never holds Server.control.
// The timer that fired stays armed meanwhile, so a member that reconnects
// after its grace period ran out does not keep the session, and one that
// disconnects again starts no second grace period. One end at a time is
// recorded, the first one asked for: the first one recorded wins anyway.
func (s *Server) recordEnd(a *attempt, value []byte, armed func() bool) {
	s.mu.Lock()
	run := !a.ended && !a.ending && !s.shutdown && armed()
	if run {
		a.ending = true
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if !run {
		return
	}
	outcome := "closed"
	if len(value) > 0 {
		outcome = string(value)
	}
	go func() {
		defer s.wg.Done()
		for wait := firstEndRetry; ; wait = min(2*wait, maxEndRetry) {
			ctx, cancel := context.WithTimeout(a.life, tryTimeout)
			err := s.endWith(ctx, a, value)
			cancel()
			if err == nil || ended(err) || a.life.Err() != nil {
				return
			}
			if wait == firstEndRetry {
				s.log.Error(err, "Ending a session failed; retrying until its end is recorded", "attempt", a.spec.UID,
					"outcome", outcome)
			}
			select {
			case <-time.After(wait):
			case <-a.life.Done():
				return
			}
		}
	}()
}

// endWith ends a session with the value of its ended key: a reason, or nothing
// to close it. It returns nil if it ended the session, and how the session
// ended if something else ended it first.
func (s *Server) endWith(ctx context.Context, a *attempt, value []byte) error {
	_, err := a.snapshot.Create(ctx, session.KeyEnded, value)
	if err == nil {
		s.end(a, value)
		return nil
	}
	if !errors.Is(err, jetstream.ErrKeyExists) {
		return err
	}
	entry, err := a.snapshot.Get(ctx, session.KeyEnded)
	if err != nil {
		return err
	}
	s.end(a, entry.Value())
	return session.EndedError(entry.Value())
}

// ended reports whether err says how a session ended.
func ended(err error) bool {
	var abort *session.AbortError
	return errors.As(err, &abort) || errors.Is(err, session.ErrClosed)
}

// end records once how a session ended, from the value of its ended key: it
// stops the session's deadline and grace timers, and reports the outcome. A
// value that is no condition reason is reported as session.ReasonInvalid, and
// logged only by its size.
func (s *Server) end(a *attempt, value []byte) {
	s.mu.Lock()
	if a.ended {
		s.mu.Unlock()
		return
	}
	a.ended = true
	a.stopTimers()
	s.mu.Unlock()
	var abort *session.AbortError
	if !errors.As(session.EndedError(value), &abort) {
		s.cfg.Observer.SessionClosed(a.spec.UID)
		s.log.Info("Session closed", "attempt", a.spec.UID)
		return
	}
	s.cfg.Observer.SessionFailed(a.spec.UID, abort.Reason)
	log := s.log.WithValues("attempt", a.spec.UID, "reason", abort.Reason)
	if abort.Reason == session.ReasonInvalid {
		log = log.WithValues("bytes", len(value))
	}
	log.Info("Session failed")
}

// discard stops everything a session runs.
func (s *Server) discard(a *attempt) {
	a.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	a.stopTimers()
	a.deadline = nil
	clear(a.grace)
}

// stopTimers stops the session's deadline and grace timers. The caller holds
// Server.mu.
func (a *attempt) stopTimers() {
	if a.deadline != nil {
		a.deadline.Stop()
	}
	for _, timer := range a.grace {
		timer.Stop()
	}
}

// lookup returns the session of an attempt, or nil.
func (s *Server) lookup(uid types.UID) *attempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[uid]
}

// isShutdown reports whether Shutdown has begun.
func (s *Server) isShutdown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdown
}

// newEpoch draws a session's epoch: random, and never zero, which a member
// presents before it has read one.
func newEpoch() (uint64, error) {
	var buf [8]byte
	for {
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, fmt.Errorf("draw an epoch: %w", err)
		}
		if epoch := binary.BigEndian.Uint64(buf[:]); epoch != 0 {
			return epoch, nil
		}
	}
}
