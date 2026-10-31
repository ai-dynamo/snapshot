// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sessiontest

import (
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/agent/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconnect breaks a member's connection while it waits: the client
// reconnects on its own, and the wait completes.
func TestReconnect(t *testing.T) {
	h := newHarness(t)

	t.Run("a waiting Get completes after reconnecting", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(2)
		sessions := ses.connectAll()
		type result struct {
			value []byte
			err   error
		}
		got := make(chan result, 1)
		go func() {
			value, err := sessions[0].Namespace(session.NamespaceCuInterpose).Get(ctx, "command", time.Minute)
			got <- result{value, err}
		}()
		time.Sleep(pending)
		ses.cut(0)
		// Inserted while the waiting member is most likely still away: its
		// watch resumes from the revision it had reached.
		require.NoError(t, sessions[1].Namespace(session.NamespaceCuInterpose).Insert(ctx, "command", []byte("dump")))
		r := <-got
		require.NoError(t, r.err)
		assert.Equal(t, "dump", string(r.value))
	})

	t.Run("a waiting Barrier completes after reconnecting", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(2)
		sessions := ses.connectAll()
		wait := barrier(ctx, sessions[0], "admitted")
		requireWaiting(t, wait)
		ses.cut(0)
		require.NoError(t, sessions[1].Barrier(ctx, "admitted"))
		require.NoError(t, <-wait)
	})

	t.Run("a write sent while reconnecting is delivered", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(2)
		sessions := ses.connectAll()
		ses.cut(0)
		require.NoError(t, sessions[0].Namespace(session.NamespaceSnapshot).Insert(ctx, "ip.0", []byte("10.0.0.1")))
		require.NoError(t, sessions[0].Leave(ctx))
		got, err := sessions[1].Namespace(session.NamespaceSnapshot).Get(ctx, "ip.0", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.1", string(got))
	})

	t.Run("the session survives repeated breaks", func(t *testing.T) {
		ctx := within(t)
		ses := h.session(2)
		sessions := ses.connectAll()
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
}

// TestCoordinatorLost restarts the coordinator under a session: a member that
// reconnects to a session recreated with another epoch, or to none, reports
// ErrCoordinatorLost.
func TestCoordinatorLost(t *testing.T) {
	t.Run("the session was recreated with another epoch", func(t *testing.T) {
		h := newHarness(t)
		ctx := within(t)
		ses := h.session(2)
		sessions := ses.connectAll()
		got := make(chan error, 1)
		go func() {
			_, err := sessions[0].Namespace(session.NamespaceSnapshot).Get(ctx, "k", time.Minute)
			got <- err
		}()
		time.Sleep(pending)
		h.restart(ses.spec)

		require.ErrorIs(t, <-got, session.ErrCoordinatorLost)
		for _, s := range sessions {
			awaitDone(t, s)
			require.ErrorIs(t, s.Err(), session.ErrCoordinatorLost)
		}
		assert.Equal(t, "failed "+session.ReasonCoordinatorLost, ses.awaitOutcome(),
			"the recreated session must be failed")
	})

	t.Run("the session is gone", func(t *testing.T) {
		h := newHarness(t)
		s := h.session(1).connect(0)
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
		s := h.session(1).connect(0)
		require.NoError(t, s.Fail(ctx, "Refused"))
		h.restart()
		require.ErrorIs(t, s.Err(), &session.AbortError{Reason: "Refused"})
	})
}
