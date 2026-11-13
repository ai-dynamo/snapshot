// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sessiontest

import (
	"fmt"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconnect breaks a member's connection while it waits: the client
// reconnects on its own, and the wait completes.
func TestReconnect(t *testing.T) {
	h := newHarness(t)

	t.Run("a waiting Get completes after reconnecting", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		type result struct {
			value []byte
			err   error
		}
		got := make(chan result, 1)
		go func() {
			value, err := sessions[0].CuInterpose().Get(ctx, "command", time.Minute)
			got <- result{value, err}
		}()
		ses.awaitWait(t, session.NamespaceCuInterpose)
		ses.cut(0)
		// Inserted while the waiting member is most likely still away: its
		// watch resumes from the revision it had reached.
		require.NoError(t, sessions[1].CuInterpose().Insert(ctx, "command", []byte("dump")))
		r := <-got
		require.NoError(t, r.err)
		assert.Equal(t, "dump", string(r.value))
	})

	t.Run("a waiting Barrier completes after reconnecting", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		wait := barrier(ctx, sessions[0], "admitted")
		requireWaiting(t, wait)
		ses.cut(0)
		require.NoError(t, sessions[1].Barrier(ctx, "admitted"))
		require.NoError(t, <-wait)
	})

	t.Run("a write sent while reconnecting is delivered", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		ses.cut(0)
		require.NoError(t, sessions[0].Snapshot().Insert(ctx, "ip.0", []byte("10.0.0.1")))
		require.NoError(t, sessions[0].Leave(ctx))
		got, err := sessions[1].Snapshot().Get(ctx, "ip.0", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.1", string(got))
	})

	t.Run("a large write sent while the coordinator refuses the member completes once it is admitted",
		func(t *testing.T) {
			ctx := within(t)
			ses := h.session(t, 2)
			sessions := ses.connectAll(t)
			h.auth.Unavailable.Store(true)
			defer h.auth.Unavailable.Store(false)
			ses.cut(0)
			value := make([]byte, 20<<20)
			inserted := make(chan error, 1)
			go func() { inserted <- sessions[0].Snapshot().Insert(ctx, "big", value) }()
			// Longer than the round trip the client gives an operation before
			// sending it again.
			requireWaitingFor(t, inserted, 4*time.Second)
			h.auth.Unavailable.Store(false)
			select {
			case err := <-inserted:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("the Insert did not return once the member was admitted")
			}
			require.NoError(t, sessions[0].Leave(ctx), "the next write")
			got, err := sessions[1].Snapshot().Get(ctx, "big", 0)
			require.NoError(t, err)
			assert.Len(t, got, len(value))
		})

	t.Run("the session survives repeated breaks", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		for i := range 3 {
			ses.cut(0)
			ses.cut(1)
			name := []string{"admitted", "net-freeze", "release"}[i]
			wait := barrier(ctx, sessions[0], name)
			require.NoError(t, sessions[1].Barrier(ctx, name))
			require.NoError(t, <-wait)
		}
		for _, s := range sessions {
			require.NoError(t, s.Err())
		}
	})

	t.Run("the most a member waits on at once stays within its buckets' consumer limits across breaks",
		func(t *testing.T) {
			ctx := within(t)
			ses := h.session(t, 2)
			sessions := ses.connectAll(t)
			get := func(ns session.Namespace, key string) <-chan error {
				out := make(chan error, 1)
				go func() {
					_, err := in(sessions[0], ns).Get(ctx, key, time.Minute)
					out <- err
				}()
				return out
			}
			// What session.MaxConsumersPerMember allows besides the end the
			// client always watches: three waits on the Snapshot bucket, and
			// one on the library bucket.
			waits := []<-chan error{
				barrier(ctx, sessions[0], "admitted"),
				get(session.NamespaceSnapshot, "ip.1"),
				get(session.NamespaceCuInterpose, "command"),
				get(session.NamespaceLibrary, "comm"),
			}
			for _, wait := range waits {
				requireWaiting(t, wait)
			}
			// Each break replaces every one of the member's consumers; a write
			// completes only once the member has reconnected.
			for i := range 2 {
				ses.cut(0)
				require.NoError(t, sessions[0].Snapshot().Insert(ctx,
					fmt.Sprintf("break.%d", i), nil))
			}
			require.NoError(t, sessions[1].Barrier(ctx, "admitted"))
			for ns, key := range map[session.Namespace]string{session.NamespaceSnapshot: "ip.1",
				session.NamespaceCuInterpose: "command", session.NamespaceLibrary: "comm"} {
				require.NoError(t, in(sessions[1], ns).Insert(ctx, key, []byte("v")))
			}
			for _, wait := range waits {
				require.NoError(t, <-wait)
			}
		})
}

// TestCoordinatorLost restarts the coordinator under a session: a member that
// reconnects to a session recreated with another epoch, or to none, reports
// ErrCoordinatorLost.
func TestCoordinatorLost(t *testing.T) {
	t.Run("the session was recreated with another epoch", func(t *testing.T) {
		h := newHarness(t)
		ctx := within(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		got := make(chan error, 1)
		go func() {
			_, err := sessions[0].Snapshot().Get(ctx, "k", time.Minute)
			got <- err
		}()
		ses.awaitWait(t, session.NamespaceSnapshot)
		h.restart(ses.spec)

		require.ErrorIs(t, <-got, session.ErrCoordinatorLost)
		for _, s := range sessions {
			awaitDone(t, s)
			require.ErrorIs(t, s.Err(), session.ErrCoordinatorLost)
		}
		assert.Equal(t, "failed "+session.ReasonCoordinatorLost, ses.awaitOutcome(t),
			"the recreated session must be failed")
	})

	t.Run("the session is gone", func(t *testing.T) {
		h := newHarness(t)
		s := h.session(t, 1).connect(t, 0)
		// The coordinator admits a member of an attempt it holds no session
		// for only if the member presents an epoch, so this also shows the
		// client presents the epoch it read when it reconnects.
		h.restart()
		awaitDone(t, s)
		require.ErrorIs(t, s.Err(), session.ErrCoordinatorLost)
		var abort *session.AbortError
		require.ErrorAs(t, s.Err(), &abort)
		assert.Equal(t, session.ReasonCoordinatorLost, abort.Reason)
	})

	t.Run("a session that ended stays ended", func(t *testing.T) {
		h := newHarness(t)
		ctx := within(t)
		s := h.session(t, 1).connect(t, 0)
		require.NoError(t, s.Fail(ctx, "Refused"))
		h.restart()
		require.ErrorIs(t, s.Err(), &session.AbortError{Reason: "Refused"})
	})
}

// TestRemoved removes a session under its connected members. Each member learns
// that its session is gone, from the ended key the coordinator records, or,
// when it has not received the key by the time the buckets go, from finding its
// session gone: every wait returns, and the session ends, with Cancelled or
// CoordinatorLost, never hanging and never reporting a closed connection.
func TestRemoved(t *testing.T) {
	t.Run("while members wait", func(t *testing.T) {
		h := newHarness(t)
		ctx := within(t)
		ses := h.session(t, 3)
		sessions := ses.connectAll(t)
		arrived := barrier(ctx, sessions[0], "admitted")
		awaitKey(t, ses.raw(t, 2), session.BarrierKey("admitted", ses.spec.Members[0].ID))
		got := make(chan error, 1)
		go func() {
			_, err := sessions[1].Library().Get(ctx, "comm", time.Minute)
			got <- err
		}()
		require.NoError(t, h.srv.Remove(ctx, ses.spec.UID))

		for _, wait := range []<-chan error{arrived, got} {
			select {
			case err := <-wait:
				requireRemoved(t, err)
			case <-ctx.Done():
				t.Fatal("a wait did not return")
			}
		}
		for _, s := range sessions {
			awaitDone(t, s)
			requireRemoved(t, s.Err())
		}
	})

	t.Run("a session that ended keeps its outcome", func(t *testing.T) {
		h := newHarness(t)
		ses := h.session(t, 1)
		require.NoError(t, ses.connect(t, 0).Fail(within(t), "Refused"))
		require.NoError(t, h.srv.Remove(within(t), ses.spec.UID))
		assert.Equal(t, []string{"opened", "failed Refused"}, h.observer.of(ses.spec.UID))
	})

	t.Run("with no room left to record its end", func(t *testing.T) {
		h := newHarness(t, func(cfg *server.Config) { cfg.MaxBytes, cfg.MaxValue = 1<<20, 64<<10 })
		ctx := within(t)
		ses := h.session(t, 2)
		sessions := ses.connectAll(t)
		// Large values, then empty ones, until neither fits: the ended key is
		// no smaller than an empty value under a longer key.
		store := sessions[0].Snapshot()
		for i, size := 0, 60<<10; size >= 0; size -= 60 << 10 {
			for ; ; i++ {
				err := store.Insert(ctx, fmt.Sprintf("fill-%d", i), make([]byte, size))
				if err != nil {
					require.ErrorIs(t, err, session.ErrTooMany)
					break
				}
			}
		}

		start := time.Now()
		require.NoError(t, h.srv.Remove(ctx, ses.spec.UID))
		assert.Equal(t, []string{"opened"}, h.observer.of(ses.spec.UID), "the end must not have been recorded")
		for _, s := range sessions {
			awaitDone(t, s)
			require.ErrorIs(t, s.Err(), session.ErrCoordinatorLost)
		}
		t.Logf("every member found its session gone %s after Remove", time.Since(start).Round(time.Millisecond))
	})
}

// requireRemoved checks that err says a removed session ended: Cancelled, if
// the member received the end, or CoordinatorLost, if it found the session gone.
func requireRemoved(t *testing.T, err error) {
	t.Helper()
	var abort *session.AbortError
	require.ErrorAs(t, err, &abort)
	assert.Contains(t, []string{session.ReasonCancelled, session.ReasonCoordinatorLost}, abort.Reason)
}
