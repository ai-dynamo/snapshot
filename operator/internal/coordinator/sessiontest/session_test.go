// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sessiontest

import (
	"context"
	"net"
	"testing"
	"time"

	client "github.com/ai-dynamo/snapshot/agent/pkg/coordinator"
	"github.com/ai-dynamo/snapshot/agent/pkg/session"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pending is how long a test watches a wait to check that it has not returned.
const pending = 200 * time.Millisecond

// barrier runs Barrier in the background, and returns its outcome on a channel.
func barrier(ctx context.Context, s client.Session, name string) <-chan error {
	out := make(chan error, 1)
	go func() { out <- s.Barrier(ctx, name) }()
	return out
}

// requireWaiting fails the test if the wait has returned.
func requireWaiting(t *testing.T, wait <-chan error) {
	t.Helper()
	select {
	case err := <-wait:
		t.Fatalf("the wait returned early: %v", err)
	case <-time.After(pending):
	}
}

// TestBarrier covers Barrier: it releases once every member has arrived, counts
// each member once, stays released, keeps an arrival whose wait was cancelled,
// and returns early with how the session ended.
func TestBarrier(t *testing.T) {
	h := newHarness(t)

	t.Run("releases only when every member has arrived", func(t *testing.T) {
		ctx := within(t)
		sessions := h.session(t, 3).connectAll(t)
		first, second := barrier(ctx, sessions[0], "admitted"), barrier(ctx, sessions[1], "admitted")
		requireWaiting(t, first)
		requireWaiting(t, second)
		require.NoError(t, sessions[2].Barrier(ctx, "admitted"))
		require.NoError(t, <-first)
		require.NoError(t, <-second)
	})

	t.Run("arriving twice counts once", func(t *testing.T) {
		ctx := within(t)
		sessions := h.session(t, 2).connectAll(t)
		first := barrier(ctx, sessions[0], "admitted")
		again := barrier(ctx, sessions[0], "admitted")
		requireWaiting(t, first)
		requireWaiting(t, again)
		require.NoError(t, sessions[1].Barrier(ctx, "admitted"))
		require.NoError(t, <-first)
		require.NoError(t, <-again)
	})

	t.Run("a released barrier returns at once", func(t *testing.T) {
		ctx := within(t)
		sessions := h.session(t, 2).connectAll(t)
		first := barrier(ctx, sessions[0], "resume")
		require.NoError(t, sessions[1].Barrier(ctx, "resume"))
		require.NoError(t, <-first)
		for _, s := range sessions {
			start := time.Now()
			require.NoError(t, s.Barrier(ctx, "resume"))
			assert.Less(t, time.Since(start), pending)
		}
	})

	t.Run("an arrival already recorded still counts after its wait is cancelled", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		waiting, cancel := context.WithCancel(ctx)
		wait := barrier(waiting, sessions[0], "net-freeze")
		awaitKey(t, ses.raw(t, 1), session.BarrierKey("net-freeze", ses.spec.Members[0].ID))
		cancel()
		require.ErrorIs(t, <-wait, context.Canceled)
		require.NoError(t, sessions[1].Barrier(ctx, "net-freeze"), "the cancelled member's arrival must count")
	})

	t.Run("a Barrier whose context is already done returns its error", func(t *testing.T) {
		ses := h.session(t, 2)
		s := ses.connect(t, 0)
		done, cancel := context.WithCancel(within(t))
		cancel()
		require.ErrorIs(t, s.Barrier(done, "net-freeze"), context.Canceled)
		_, err := ses.raw(t, 1).Get(within(t), session.BarrierKey("net-freeze", ses.spec.Members[0].ID))
		require.ErrorIs(t, err, jetstream.ErrKeyNotFound, "the arrival must not be recorded")
	})

	t.Run("cancelling a waiting barrier returns at once", func(t *testing.T) {
		ctx := within(t)
		sessions := h.session(t, 2).connectAll(t)
		waiting, cancel := context.WithCancel(ctx)
		wait := barrier(waiting, sessions[0], "release")
		requireWaiting(t, wait)
		cancel()
		require.ErrorIs(t, <-wait, context.Canceled)
		require.NoError(t, sessions[1].Barrier(ctx, "release"))
	})

	t.Run("barriers are independent", func(t *testing.T) {
		ctx := within(t)
		sessions := h.session(t, 2).connectAll(t)
		wait := barrier(ctx, sessions[0], "one")
		second := barrier(ctx, sessions[1], "two")
		requireWaiting(t, wait)
		requireWaiting(t, second)
		require.NoError(t, sessions[1].Barrier(ctx, "one"))
		require.NoError(t, <-wait)
		require.NoError(t, sessions[0].Barrier(ctx, "two"))
		require.NoError(t, <-second)
	})

	t.Run("every waiter returns when the session fails", func(t *testing.T) {
		ctx := within(t)
		sessions := h.session(t, 3).connectAll(t)
		first, second := barrier(ctx, sessions[0], "admitted"), barrier(ctx, sessions[1], "admitted")
		requireWaiting(t, first)
		require.NoError(t, sessions[2].Fail(ctx, "Refused"))
		require.ErrorIs(t, <-first, &session.AbortError{Reason: "Refused"})
		require.ErrorIs(t, <-second, &session.AbortError{Reason: "Refused"})
	})

	t.Run("every waiter returns when the session closes", func(t *testing.T) {
		ctx := within(t)
		sessions := h.session(t, 2).connectAll(t)
		first, second := barrier(ctx, sessions[0], "admitted"), barrier(ctx, sessions[1], "other")
		requireWaiting(t, first)
		for _, s := range sessions {
			require.NoError(t, s.Leave(ctx))
		}
		require.ErrorIs(t, <-first, session.ErrClosed)
		require.ErrorIs(t, <-second, session.ErrClosed)
	})

	t.Run("a name must be one token", func(t *testing.T) {
		s := h.session(t, 1).connect(t, 0)
		require.ErrorIs(t, s.Barrier(within(t), "a.b"), session.ErrInvalid)
		require.ErrorIs(t, s.Barrier(within(t), "*"), session.ErrInvalid)
	})
}

// TestLeave covers Leave: leaving twice changes nothing, a member that has left
// keeps using the session, and the session closes once every member has left.
func TestLeave(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	ses := h.session(t, 2)
	sessions := ses.connectAll(t)
	require.NoError(t, sessions[0].Leave(ctx))
	require.NoError(t, sessions[0].Leave(ctx), "leaving twice")
	require.NoError(t, sessions[0].Namespace(session.NamespaceLibrary).Insert(ctx, "after", []byte("left")),
		"a member that left keeps its connection")
	require.NoError(t, sessions[0].Err())
	require.NoError(t, sessions[1].Leave(ctx))
	for _, s := range sessions {
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), session.ErrClosed)
	}
	assert.Equal(t, "closed", ses.awaitOutcome(t))
}

// TestSessionFail covers Fail: the first reason wins, and every later Fail
// reports it.
func TestSessionFail(t *testing.T) {
	h := newHarness(t)

	t.Run("the first reason wins", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(t, 3)
		sessions := ses.connectAll(t)
		require.NoError(t, sessions[0].Fail(ctx, "PodIPChanged"))
		require.ErrorIs(t, sessions[1].Fail(ctx, "MemberFailed"), &session.AbortError{Reason: "PodIPChanged"},
			"a second Fail reports the first reason")
		require.NoError(t, sessions[2].Fail(ctx, "PodIPChanged"), "failing with the same reason is no error")
		assert.Equal(t, "failed PodIPChanged", ses.awaitOutcome(t))
		for _, s := range sessions {
			awaitDone(t, s)
			require.ErrorIs(t, s.Err(), &session.AbortError{Reason: "PodIPChanged"})
		}
	})

	t.Run("failing again is no error", func(t *testing.T) {
		ctx := within(t)
		s := h.session(t, 1).connect(t, 0)
		require.NoError(t, s.Fail(ctx, "Refused"))
		require.NoError(t, s.Fail(ctx, "Refused"))
	})

	t.Run("every operation after the end reports it", func(t *testing.T) {
		ctx := within(t)
		sessions := h.session(t, 2).connectAll(t)
		require.NoError(t, sessions[0].Fail(ctx, "Refused"))
		awaitDone(t, sessions[1])
		abort := &session.AbortError{Reason: "Refused"}
		require.ErrorIs(t, sessions[1].Namespace(session.NamespaceSnapshot).Insert(ctx, "k", nil), abort)
		_, err := sessions[1].Namespace(session.NamespaceLibrary).Get(ctx, "k", time.Minute)
		require.ErrorIs(t, err, abort)
		require.ErrorIs(t, sessions[1].Barrier(ctx, "admitted"), abort)
		require.ErrorIs(t, sessions[1].Leave(ctx), abort)
	})

	t.Run("a session that closed reports it", func(t *testing.T) {
		ctx := within(t)
		s := h.session(t, 1).connect(t, 0)
		require.NoError(t, s.Leave(ctx))
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), session.ErrClosed)
		require.ErrorIs(t, s.Fail(ctx, "Late"), session.ErrClosed)
	})

	t.Run("an ended key that holds no reason reports InvalidReason", func(t *testing.T) {
		ses := h.session(t, 2)
		s := ses.connect(t, 0)
		_, err := ses.raw(t, 1).Create(within(t), session.KeyEnded, []byte("not a reason\x00"))
		require.NoError(t, err)
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), &session.AbortError{Reason: session.ReasonInvalid})
	})

	t.Run("a reason must be a condition reason", func(t *testing.T) {
		s := h.session(t, 1).connect(t, 0)
		for _, reason := range []string{"", "has space", "1Digit", "Trailing:"} {
			require.ErrorIs(t, s.Fail(within(t), reason), session.ErrInvalid, "reason %q", reason)
		}
		require.NoError(t, s.Err())
	})
}

// TestConnect covers Connect: it waits while the coordinator refuses it, reads
// Meta, and refuses a member the session does not have.
func TestConnect(t *testing.T) {
	h := newHarness(t)

	t.Run("reads meta", func(t *testing.T) {
		ses := h.session(t, 3)
		meta := ses.connect(t, 1).Meta()
		assert.Equal(t, ses.spec.Members, meta.Members)
		assert.True(t, ses.spec.Deadline.Equal(meta.Deadline))
		assert.NotZero(t, meta.Epoch)
		assert.Equal(t, session.MaxValue, meta.MaxValue)
		assert.Equal(t, session.MaxLibraryValue, meta.MaxLibraryValue)
	})

	t.Run("waits while the coordinator refuses it", func(t *testing.T) {
		ses := h.session(t, 1)
		h.auth.refuse.Store(true)
		defer h.auth.refuse.Store(false)
		connected := make(chan error, 1)
		go func() {
			s, err := client.Connect(within(t), ses.config(0))
			if err == nil {
				defer func() { _ = s.Close() }()
			}
			connected <- err
		}()
		requireWaiting(t, connected)
		h.auth.refuse.Store(false)
		require.NoError(t, <-connected)
	})

	t.Run("reports a refusal that lasts", func(t *testing.T) {
		ses := h.session(t, 1)
		h.auth.refuse.Store(true)
		defer h.auth.refuse.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := client.Connect(ctx, ses.config(0))
		require.ErrorIs(t, err, session.ErrForbidden)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("refuses a member the session does not have", func(t *testing.T) {
		cfg := h.session(t, 1).config(0)
		cfg.Member = "stranger"
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := client.Connect(ctx, cfg)
		require.ErrorIs(t, err, session.ErrForbidden)
	})

	t.Run("accepts several URLs", func(t *testing.T) {
		ses := h.session(t, 1)
		cfg := ses.config(0)
		cfg.URLs = []string{"tls://127.0.0.1:1", cfg.URLs[0]}
		s, err := client.Connect(within(t), cfg)
		require.NoError(t, err)
		require.NoError(t, s.Close())
	})

	for _, tt := range []struct {
		name   string
		modify func(*client.Config)
	}{
		{"no URL", func(c *client.Config) { c.URLs = nil }},
		{"no CA pool", func(c *client.Config) { c.RootCAs = nil }},
		{"no token source", func(c *client.Config) { c.Token = nil }},
		{"an attempt that is not a name", func(c *client.Config) { c.Attempt = "a.b" }},
		{"a member that is not a name", func(c *client.Config) { c.Member = "a/b" }},
	} {
		t.Run("refuses a config with "+tt.name, func(t *testing.T) {
			cfg := h.session(t, 1).config(0)
			tt.modify(&cfg)
			_, err := client.Connect(within(t), cfg)
			require.ErrorIs(t, err, session.ErrInvalid)
		})
	}
}

// TestClose covers Close: it closes the connection and nothing else, so the
// coordinator fails the session once the member's grace period runs out.
func TestClose(t *testing.T) {
	h := newHarness(t, func(cfg *coordinator.Config) { cfg.Grace = 2 * time.Second })
	ctx := within(t)
	ses := h.session(t, 2)
	sessions := ses.connectAll(t)
	wait := barrier(ctx, sessions[0], "admitted")
	requireWaiting(t, wait)
	require.NoError(t, sessions[0].Close())
	require.ErrorIs(t, <-wait, net.ErrClosed)
	require.ErrorIs(t, sessions[0].Namespace(session.NamespaceSnapshot).Insert(ctx, "k", nil), net.ErrClosed)
	require.NoError(t, sessions[0].Close(), "closing twice")
	select {
	case <-sessions[0].Done():
		t.Fatal("closing must not end the session")
	default:
	}
	assert.Equal(t, []string{"opened"}, h.observer.of(ses.spec.UID), "closing must not end the session")
	require.NoError(t, sessions[1].Err())
	awaitDone(t, sessions[1])
	require.ErrorIs(t, sessions[1].Err(), &session.AbortError{Reason: session.ReasonMemberFailed})
}
