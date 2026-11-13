// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sessiontest

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pending is how long a test watches a wait to check that it has not returned.
const pending = 200 * time.Millisecond

// barrier runs Barrier in the background, and returns its outcome on a channel.
func barrier(ctx context.Context, s coordinator.Session, name string) <-chan error {
	out := make(chan error, 1)
	go func() { out <- s.Barrier(ctx, name) }()
	return out
}

// requireWaiting fails the test if the wait has returned.
func requireWaiting(t *testing.T, wait <-chan error) {
	t.Helper()
	requireWaitingFor(t, wait, pending)
}

// requireWaitingFor fails the test if the wait returns within d.
func requireWaitingFor(t *testing.T, wait <-chan error, d time.Duration) {
	t.Helper()
	select {
	case err := <-wait:
		t.Fatalf("the wait returned early: %v", err)
	case <-time.After(d):
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
	})
}

// TestWaitsShareTheConsumerLimit covers a bucket's consumer limit, which a
// session's members share: one member's waits that take every consumer delay
// another member's Barrier, and their own waits past the limit, and fail none
// of them; each completes once a consumer frees.
func TestWaitsShareTheConsumerLimit(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	ses := h.session(t, 2)
	sessions := ses.connectAll(t)
	snapshot := sessions[0].Snapshot()
	gets := make([]<-chan error, 2*session.MaxConsumersPerMember)
	for i := range gets {
		got := make(chan error, 1)
		go func() {
			_, err := snapshot.Get(ctx, fmt.Sprintf("peer.%d", i), time.Minute)
			got <- err
		}()
		gets[i] = got
	}
	ses.awaitMostConsumers(t)
	wait := barrier(ctx, sessions[1], "admitted")
	requireWaiting(t, wait)
	for i := range gets {
		require.NoError(t, sessions[1].Snapshot().Insert(ctx, fmt.Sprintf("peer.%d", i), nil))
	}
	for i, got := range gets {
		select {
		case err := <-got:
			require.NoError(t, err, "Get %d", i)
		case <-ctx.Done():
			t.Fatalf("Get %d did not return", i)
		}
	}
	require.NoError(t, sessions[0].Barrier(ctx, "admitted"))
	require.NoError(t, <-wait)
}

// TestWaitsFreeTheirConsumers checks that a wait that ends deletes its watch's
// consumer at once, on each bucket, instead of leaving it to expire after the
// coordinator's five seconds. A consumer left behind changes nothing a wait
// returns, so the test counts each bucket's consumers, which a member's
// connection may read.
func TestWaitsFreeTheirConsumers(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	ses := h.session(t, 2)
	sessions := ses.connectAll(t)
	uid := string(ses.spec.UID)
	snapshot, library := session.SnapshotBucket(uid), session.LibraryBucket(uid)
	// The coordinator watches the departures, and each member the session's
	// end.
	idle := len(sessions) + 1
	ses.awaitConsumers(t, snapshot, idle)
	freed := func(bucket string, n int) {
		t.Helper()
		ses.consumersWithin(t, bucket, time.Second, func(held, _ int) bool { return held == n })
	}

	got := make(chan error, 1)
	go func() {
		_, err := sessions[0].Snapshot().Get(ctx, "k", time.Minute)
		got <- err
	}()
	ses.awaitConsumers(t, snapshot, idle+1)
	require.NoError(t, sessions[1].Snapshot().Insert(ctx, "k", nil))
	require.NoError(t, <-got)
	freed(snapshot, idle)

	wait := barrier(ctx, sessions[0], "admitted")
	ses.awaitConsumers(t, snapshot, idle+1)
	require.NoError(t, sessions[1].Barrier(ctx, "admitted"))
	require.NoError(t, <-wait)
	freed(snapshot, idle)

	go func() {
		_, err := sessions[0].Library().Get(ctx, "comm", time.Minute)
		got <- err
	}()
	ses.awaitConsumers(t, library, 1)
	require.NoError(t, sessions[1].Library().Insert(ctx, "comm", nil))
	require.NoError(t, <-got)
	freed(library, 0)
}

// TestLeave covers Leave: leaving twice changes nothing, a member that has left
// keeps using the session, and the session closes once every member has left,
// after which leaving still succeeds.
func TestLeave(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	ses := h.session(t, 2)
	sessions := ses.connectAll(t)
	require.NoError(t, sessions[0].Leave(ctx))
	require.NoError(t, sessions[0].Leave(ctx), "leaving twice")
	require.NoError(t, sessions[0].Library().Insert(ctx, "after", []byte("left")),
		"a member that left keeps its connection")
	require.NoError(t, sessions[0].Err())
	require.NoError(t, sessions[1].Leave(ctx))
	for _, s := range sessions {
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), session.ErrClosed)
		require.NoError(t, s.Leave(ctx), "leaving a session that closed")
	}
	assert.Equal(t, "closed", ses.awaitOutcome(t))
}

// TestLeaveThatClosesTheSession covers the last member's Leave, whose own
// departure closes the session: the client may see the session end before
// the reply to its write, and Leave still succeeds. Each session gives the
// race one chance, so the test runs many.
func TestLeaveThatClosesTheSession(t *testing.T) {
	h := newHarness(t)
	for i := range 50 {
		sessions := h.session(t, 2).connectAll(t)
		for member, s := range sessions {
			require.NoError(t, s.Leave(within(t)), "session %d, member %d", i, member)
		}
		for _, s := range sessions {
			awaitDone(t, s)
			require.ErrorIs(t, s.Err(), session.ErrClosed)
			require.NoError(t, s.Close())
		}
	}
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
		require.ErrorIs(t, sessions[1].Snapshot().Insert(ctx, "k", nil), abort)
		_, err := sessions[1].Library().Get(ctx, "k", time.Minute)
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

	t.Run("a reason must be a condition reason", func(t *testing.T) {
		s := h.session(t, 1).connect(t, 0)
		require.ErrorIs(t, s.Fail(within(t), "has space"), session.ErrInvalid)
		require.NoError(t, s.Err())
	})
}

// TestConnect covers Connect: it waits while the coordinator refuses it, reads
// Meta, refuses a member the session does not have, and retries a handshake
// whose CAs it could not read.
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

	t.Run("knows its member's rank", func(t *testing.T) {
		ses := h.session(t, 3)
		for _, rank := range []int{2, 0, 1} {
			assert.Equal(t, rank, ses.connect(t, rank).Rank(), "member-%d", rank)
		}
	})

	t.Run("waits while the coordinator refuses it", func(t *testing.T) {
		ses := h.session(t, 1)
		h.auth.Unavailable.Store(true)
		defer h.auth.Unavailable.Store(false)
		connected := make(chan error, 1)
		go func() {
			s, err := coordinator.Connect(within(t), ses.config(0))
			if err == nil {
				defer func() { _ = s.Close() }()
			}
			connected <- err
		}()
		requireWaiting(t, connected)
		h.auth.Unavailable.Store(false)
		require.NoError(t, <-connected)
	})

	t.Run("reports a refusal that lasts", func(t *testing.T) {
		ses := h.session(t, 1)
		h.auth.Unavailable.Store(true)
		defer h.auth.Unavailable.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := coordinator.Connect(ctx, ses.config(0))
		require.ErrorIs(t, err, session.ErrForbidden)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("connects again once it can read its CAs", func(t *testing.T) {
		ses := h.session(t, 1)
		tr := newTrust(h.ca)
		s := ses.connectTrusting(t, 0, tr)
		tr.unreadable.Store(true)
		ses.cutUntilRefused(t, 0, tr)
		assert.True(t, slices.ContainsFunc(h.logs.all(), func(line string) bool {
			return strings.Contains(line, "Reading the coordinator's CA failed")
		}), "the failed read must be logged")
		tr.unreadable.Store(false)
		require.NoError(t, s.Snapshot().Insert(within(t), "k", []byte("v")))
		require.NoError(t, s.Err())
	})

	t.Run("refuses a member the session does not have", func(t *testing.T) {
		cfg := h.session(t, 1).config(0)
		cfg.Member = "stranger"
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := coordinator.Connect(ctx, cfg)
		require.ErrorIs(t, err, session.ErrForbidden)
	})

	t.Run("accepts several URLs", func(t *testing.T) {
		ses := h.session(t, 1)
		cfg := ses.config(0)
		cfg.URLs = []string{"tls://127.0.0.1:1", cfg.URLs[0]}
		s, err := coordinator.Connect(within(t), cfg)
		require.NoError(t, err)
		require.NoError(t, s.Close())
	})

	for _, tt := range []struct {
		name   string
		modify func(*coordinator.Config)
	}{
		{"no URL", func(c *coordinator.Config) { c.URLs = nil }},
		{"no CA source", func(c *coordinator.Config) { c.RootCAs = nil }},
		{"a CA source that returns no pool", func(c *coordinator.Config) {
			c.RootCAs = func() (*x509.CertPool, error) { return nil, nil }
		}},
		{"no token source", func(c *coordinator.Config) { c.Token = nil }},
		{"an attempt too long for its buckets", func(c *coordinator.Config) {
			c.Attempt = strings.Repeat("u", session.MaxAttemptLength+1)
		}},
		{"a member that is not a name", func(c *coordinator.Config) { c.Member = "a/b" }},
	} {
		t.Run("refuses a config with "+tt.name, func(t *testing.T) {
			cfg := h.session(t, 1).config(0)
			tt.modify(&cfg)
			_, err := coordinator.Connect(within(t), cfg)
			require.ErrorIs(t, err, session.ErrInvalid)
		})
	}
}

// TestClose covers Close: it closes the connection and nothing else, so the
// coordinator fails the session once the member's grace period runs out.
func TestClose(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	ses := h.session(t, 2)
	sessions := ses.connectAll(t)
	wait := barrier(ctx, sessions[0], "admitted")
	requireWaiting(t, wait)
	require.NoError(t, sessions[0].Close())
	require.ErrorIs(t, <-wait, net.ErrClosed)
	require.ErrorIs(t, sessions[0].Snapshot().Insert(ctx, "k", nil), net.ErrClosed)
	require.NoError(t, sessions[0].Close(), "closing twice")
	select {
	case <-sessions[0].Done():
		t.Fatal("closing must not end the session")
	default:
	}
	assert.Equal(t, []string{"opened"}, h.observer.of(ses.spec.UID), "closing must not end the session")
	require.NoError(t, sessions[1].Err())
	h.clock.Await(t, server.DefaultGrace)
	h.clock.Step(server.DefaultGrace)
	awaitDone(t, sessions[1])
	require.ErrorIs(t, sessions[1].Err(), &session.AbortError{Reason: session.ReasonMemberFailed})
}

// TestCloseWhileWatching is a stress test. It closes members while their
// watches may still be being created: the one that follows the session's end,
// which Connect starts, and a Barrier's. nats.go may never close the channel
// of a watch cancelled as it is created (nats-io/nats.go#2223), so a client
// that waited for that channel would never return from Close or the Barrier.
// Nothing makes a Close land in that window, which only a hook in production
// code could hold open, so each Close gives the race one chance and the test
// closes many members. The window is narrow: before the fix, the test failed
// in 2 of 8 concurrent runs of 100 under -race, and rarely on an idle machine.
func TestCloseWhileWatching(t *testing.T) {
	h := newHarness(t)
	for i := range 50 {
		ctx := within(t)
		s, err := coordinator.Connect(ctx, h.session(t, 2).config(0))
		require.NoError(t, err)
		wait := barrier(ctx, s, "never")
		closed := make(chan error, 1)
		go func() { closed <- s.Close() }()
		select {
		case err := <-closed:
			require.NoError(t, err, "session %d", i)
		case <-ctx.Done():
			t.Fatalf("session %d: Close did not return", i)
		}
		select {
		case err := <-wait:
			require.ErrorIs(t, err, net.ErrClosed, "session %d", i)
		case <-ctx.Done():
			t.Fatalf("session %d: the Barrier did not return", i)
		}
	}
}
