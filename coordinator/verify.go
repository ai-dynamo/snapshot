// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go/jetstream"
)

// target returns how many verifications the session must have passed: one
// for each request, and one for each reconnect, which nats.go counts before
// the new connection delivers anything. The caller holds s.mu.
func (s *client) target() uint64 { return s.requested + s.nc.Stats().Reconnects }

// request asks for the session to be verified, as whatever may have found the
// session gone does: a watch that stopped on its own, or an operation that
// found its bucket missing. It returns the target a verification answers it
// by reaching.
func (s *client) request() uint64 {
	s.mu.Lock()
	s.requested++
	target := s.target()
	s.mu.Unlock()
	s.wakeVerifier()
	return target
}

// wakeVerifier has the verifier run, unless it is already due to.
func (s *client) wakeVerifier() {
	select {
	case s.verify <- struct{}{}:
	default:
	}
}

// verifier verifies the session whenever asked, one verification at a time.
func (s *client) verifier() {
	defer s.wg.Done()
	for {
		select {
		case <-s.verify:
			s.check()
		case <-s.closing:
			return
		case <-s.done:
			return
		}
	}
}

// check verifies the session by reading Meta again. The same epoch means the
// coordinator still holds the session the member joined, which answers every
// request and reconnect before check began. Another epoch, or no Meta, means it
// recreated the session after losing it, and no bucket means it no longer
// holds the session at all: either way, the session ends with
// session.ErrCoordinatorLost, after failing a recreated session with that
// reason.
func (s *client) check() {
	for {
		s.mu.Lock()
		target := s.target()
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		_, err := s.snapshot.Status(ctx)
		var meta session.Meta
		if err == nil {
			meta, err = readMeta(ctx, s.snapshot)
		}
		cancel()
		switch {
		case err == nil && meta.Epoch == s.meta.Epoch:
			s.settled(target)
			return
		case err == nil, errors.Is(err, session.ErrNotFound):
			s.lose(true)
			return
		case errors.Is(err, jetstream.ErrStreamNotFound), errors.Is(err, jetstream.ErrBucketNotFound):
			s.lose(false)
			return
		}
		select {
		case <-time.After(retryWait):
		case <-s.closing:
			return
		case <-s.done:
			return
		}
	}
}

// lose ends a session the coordinator lost. If the coordinator recreated it,
// the session first fails the recreated one, so every member learns at once;
// creating ended fails only if something ended it already, which serves as
// well.
func (s *client) lose(recreated bool) {
	if recreated {
		ctx, cancel := context.WithTimeout(context.Background(), attemptTimeout)
		_, _ = s.snapshot.Create(ctx, session.KeyEnded, []byte(session.ReasonCoordinatorLost))
		cancel()
	}
	s.log.Info("The coordinator lost the session")
	s.finish(session.ErrCoordinatorLost)
}

// settled records that a verification that reached target found the session
// still there, and wakes every caller waiting for it.
func (s *client) settled(target uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if target > s.verified {
		s.verified = target
		close(s.wake)
		s.wake = make(chan struct{})
	}
}

// confirm has the session verified, and returns nil once a verification
// answering its request has found it still there, or how it ended.
func (s *client) confirm(ctx context.Context) error { return s.verifiedTo(ctx, s.request()) }

// settle returns once the session has been verified as far as was due when
// settle was called: every reconnect and request until then. A wait calls it
// before returning what it read, so it never reports a value or a release it
// read from a session the coordinator recreated: after a coordinator restart,
// a watch resumes on the recreated bucket, and it may deliver an entry before
// the verification of the reconnect has ended the session.
func (s *client) settle(ctx context.Context) error {
	s.mu.Lock()
	target := s.target()
	s.mu.Unlock()
	return s.verifiedTo(ctx, target)
}

// verifiedTo returns nil once a verification that reached target has found
// the session still there, and how the session ended if it ends first.
func (s *client) verifiedTo(ctx context.Context, target uint64) error {
	for {
		s.mu.Lock()
		ok, wake := s.verified >= target, s.wake
		s.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-wake:
		case <-s.done:
			return s.Err()
		case <-s.closing:
			return net.ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
