// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sessiontest

import (
	"context"
	"crypto/tls"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/internal/testcert"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests of what the coordinator does to a session, as its members see it:
// the deadline, the grace period, the host failing, removing, and shutting
// down, and TLS rotation. Each test that steps the clock has a coordinator of
// its own, because the members a test leaves connected start grace periods on
// its clock when they close.

// TestDeadline covers the deadline: the coordinator fails the session at it,
// and a member that cannot learn how its session ended ends the session itself
// DeadlineSlack past it, unless the session ended first.
func TestDeadline(t *testing.T) {
	t.Run("the coordinator fails the session at its deadline", func(t *testing.T) {
		h := newHarness(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		h.clock.Step(time.Hour)
		for _, s := range sessions {
			awaitDone(t, s)
			require.ErrorIs(t, s.Err(), &session.AbortError{Reason: session.ReasonDeadlineExceeded})
		}
		assert.Equal(t, "failed "+session.ReasonDeadlineExceeded, ses.awaitOutcome(t))
	})

	t.Run("a session past its deadline starts failed", func(t *testing.T) {
		h := newHarness(t)
		ses := h.sessionUntil(t, 1, h.clock.Now().Add(-time.Second))
		s := ses.connect(t, 0)
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), &session.AbortError{Reason: session.ReasonDeadlineExceeded})
		assert.Equal(t, []string{"opened", "failed " + session.ReasonDeadlineExceeded}, h.observer.of(ses.spec.UID))
	})

	t.Run("a member the coordinator refuses ends its session itself", func(t *testing.T) {
		h := newHarness(t)
		ses := h.session(t, 1)
		s := ses.connect(t, 0)
		h.auth.Unavailable.Store(true)
		ses.cut(0)
		// Nothing bounds the Barrier but the session.
		wait := barrier(context.Background(), s, "admitted")
		h.clock.Step(time.Hour)
		requireWaiting(t, wait)
		h.clock.Step(coordinator.DeadlineSlack)
		select {
		case err := <-wait:
			require.ErrorIs(t, err, &session.AbortError{Reason: session.ReasonDeadlineExceeded})
		case <-within(t).Done():
			t.Fatal("the Barrier did not return")
		}
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), &session.AbortError{Reason: session.ReasonDeadlineExceeded})
	})

	t.Run("a member that joins past the deadline and its slack ends its session at once", func(t *testing.T) {
		h := newHarness(t)
		ses := h.sessionUntil(t, 1, h.clock.Now().Add(-2*coordinator.DeadlineSlack))
		// No logger: the harness's, which every client shares, has a lock,
		// which would order the deadline's goroutine after join.
		cfg := ses.config(0)
		cfg.Log = logr.Logger{}
		s, err := coordinator.Connect(within(t), cfg)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), &session.AbortError{Reason: session.ReasonDeadlineExceeded})
	})

	t.Run("a session that ended first keeps its end", func(t *testing.T) {
		h := newHarness(t)
		s := h.session(t, 1).connect(t, 0)
		require.NoError(t, s.Fail(within(t), "Refused"))
		h.clock.Step(time.Hour + coordinator.DeadlineSlack)
		require.ErrorIs(t, s.Err(), &session.AbortError{Reason: "Refused"})
	})
}

// TestGrace covers the grace period: a member that reconnects within it keeps
// the session, one that does not fails it, even after Leave, and a session
// that ended, or a member with another connection open, starts none.
func TestGrace(t *testing.T) {
	t.Run("a member that reconnects within its grace period keeps the session", func(t *testing.T) {
		h := newHarness(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		ses.cut(0)
		h.clock.Await(t, server.DefaultGrace)
		h.clock.AwaitStop(t, server.DefaultGrace)
		h.clock.Step(server.DefaultGrace)
		require.NoError(t, sessions[0].Snapshot().Insert(within(t), "k", nil))
		for _, s := range sessions {
			require.NoError(t, s.Err())
		}
		assert.Equal(t, []string{"opened"}, h.observer.of(ses.spec.UID))
	})

	t.Run("a member that does not reconnect fails the session when its grace period runs out", func(t *testing.T) {
		h := newHarness(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		h.auth.Unavailable.Store(true)
		ses.cut(0)
		h.clock.Await(t, server.DefaultGrace)
		h.clock.Step(server.DefaultGrace - time.Millisecond)
		requireOpen(t, sessions[1], "the grace period has not run out yet")
		h.clock.Step(time.Millisecond)
		awaitDone(t, sessions[1])
		require.ErrorIs(t, sessions[1].Err(), &session.AbortError{Reason: session.ReasonMemberFailed})
		assert.Equal(t, "failed "+session.ReasonMemberFailed, ses.awaitOutcome(t))
	})

	t.Run("a member that left still gets a grace period", func(t *testing.T) {
		h := newHarness(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		require.NoError(t, sessions[0].Leave(within(t)))
		require.NoError(t, sessions[0].Close())
		h.clock.Await(t, server.DefaultGrace)
		h.clock.Step(server.DefaultGrace)
		awaitDone(t, sessions[1])
		require.ErrorIs(t, sessions[1].Err(), &session.AbortError{Reason: session.ReasonMemberFailed})
	})

	t.Run("a session that ended starts none", func(t *testing.T) {
		h := newHarness(t)
		sessions := h.session(t, 2).connectAll(t)
		require.NoError(t, sessions[0].Fail(within(t), "Refused"))
		for _, s := range sessions {
			awaitDone(t, s)
		}
		requireNoGrace(t, h)
	})

	t.Run("a member keeps its session while one of its connections is open", func(t *testing.T) {
		h := newHarness(t)
		ses := h.session(t, 1)
		s := ses.connect(t, 0)
		extra := ses.connect(t, 0)
		require.NoError(t, extra.Close())
		requireNoGrace(t, h)
		require.NoError(t, s.Err())
	})
}

// requireOpen fails the test if the session ends within pending.
func requireOpen(t *testing.T, s coordinator.Session, msg string) {
	t.Helper()
	select {
	case <-s.Done():
		t.Fatalf("%s, but the session ended: %v", msg, s.Err())
	case <-time.After(pending):
	}
}

// requireNoGrace fails the test if the coordinator starts a grace period
// within pending.
func requireNoGrace(t *testing.T, h *harness) {
	t.Helper()
	require.Never(t, func() bool { return h.clock.Armed(server.DefaultGrace) > 0 }, pending, 5*time.Millisecond,
		"no grace period may start")
}

// TestCoordinatorFail covers the host failing a session on the coordinator's
// own authority: every member learns the reason, the first reason wins, and
// the reason must be a condition reason.
func TestCoordinatorFail(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	ses := h.session(t, 2)
	sessions := ses.connectAll(t)
	uid := ses.spec.UID
	require.ErrorIs(t, h.srv.Fail(ctx, uid, "has space"), session.ErrInvalid)
	require.NoError(t, h.srv.Fail(ctx, uid, session.ReasonMemberFailed))
	require.NoError(t, h.srv.Fail(ctx, uid, session.ReasonMemberFailed), "failing again with the same reason")
	require.ErrorIs(t, h.srv.Fail(ctx, uid, "Other"), &session.AbortError{Reason: session.ReasonMemberFailed},
		"the first reason wins")
	for _, s := range sessions {
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), &session.AbortError{Reason: session.ReasonMemberFailed})
	}
	assert.Equal(t, "failed "+session.ReasonMemberFailed, ses.awaitOutcome(t))
	require.ErrorIs(t, h.srv.Fail(ctx, "unknown", "Refused"), session.ErrNotFound)
}

// TestShutdown covers shutting down: every open session fails with
// CoordinatorLost, one that ended keeps its end, and the coordinator refuses
// everything afterwards.
func TestShutdown(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	open, finished := h.session(t, 2), h.session(t, 1)
	sessions := open.connectAll(t)
	require.NoError(t, finished.connect(t, 0).Fail(ctx, "Refused"))
	assert.Equal(t, "failed Refused", finished.awaitOutcome(t))

	addr := h.srv.Addr().String()
	require.NoError(t, h.srv.Shutdown(ctx))
	for _, s := range sessions {
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), session.ErrCoordinatorLost)
	}
	assert.Equal(t, "failed "+session.ReasonCoordinatorLost, open.awaitOutcome(t))
	assert.Len(t, h.observer.of(finished.spec.UID), 2, "a session that ended is not failed again")
	require.Error(t, h.srv.Ensure(ctx, server.Spec{UID: "after", Members: open.spec.Members,
		Deadline: open.spec.Deadline}))
	require.Error(t, h.srv.Remove(ctx, open.spec.UID))
	require.NoError(t, h.srv.Shutdown(ctx), "shutting down twice")
	_, err := net.DialTimeout("tcp", addr, time.Second)
	require.Error(t, err, "the listener must be closed")
}

// TestTLSRotation replaces the coordinator's CA without restarting anything:
// the coordinator serves the certificate the new CA signed to new connections,
// and a member, which reads its CAs on every handshake, connects again once it
// trusts the new CA, and keeps its session.
func TestTLSRotation(t *testing.T) {
	oldCA, oldCert := testcert.New(t)
	newCA, newCert := testcert.New(t)
	var current atomic.Pointer[tls.Certificate]
	current.Store(&oldCert)
	h := newHarness(t, func(cfg *server.Config) {
		cfg.TLS = &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return current.Load(), nil
			},
		}
	})
	ses := h.session(t, 2)
	rotated := newTrust(oldCA)
	before := ses.connectTrusting(t, 0, rotated)
	current.Store(&newCert)
	after := ses.connectTrusting(t, 1, newTrust(newCA))
	require.NoError(t, before.Snapshot().Insert(within(t), "k", []byte("v")))
	got, err := after.Snapshot().Get(within(t), "k", 0)
	require.NoError(t, err)
	assert.Equal(t, "v", string(got))

	// The member that trusts only the old CA is refused once its connection
	// breaks, until it reads the new CA.
	ses.cutUntilRefused(t, 0, rotated)
	rotated.pool.Store(newCA)
	require.NoError(t, before.Snapshot().Insert(within(t), "k2", []byte("v2")))
	got, err = after.Snapshot().Get(within(t), "k2", 0)
	require.NoError(t, err)
	assert.Equal(t, "v2", string(got))
	require.NoError(t, before.Err())
}
