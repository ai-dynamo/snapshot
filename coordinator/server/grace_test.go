// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ensureForGrace ensures an attempt with n members, and consumes the report of
// its deadline timer, so the next timer the clock reports is a grace period.
func (h *harness) ensureForGrace(n int) Spec {
	h.t.Helper()
	spec := h.ensure(n)
	require.Equal(h.t, time.Hour, h.clock.AwaitTimer(h.t))
	return spec
}

// requireNoGrace checks that no member of spec has a grace period running.
func (h *harness) requireNoGrace(spec Spec) {
	h.t.Helper()
	time.Sleep(pending)
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	assert.Empty(h.t, h.srv.attempts[spec.UID].grace)
}

// TestGrace covers the grace period: a member that reconnects within it keeps
// the session, and one that does not fails it. Each case has a coordinator of
// its own, because the members a case leaves connected start grace periods on
// its clock when they close.
func TestGrace(t *testing.T) {
	t.Run("a member that reconnects within its grace period keeps the session", func(t *testing.T) {
		h := newHarness(t)
		spec := h.ensureForGrace(2)
		members := h.connectAll(spec)
		h.kick(spec, 0)
		require.Equal(t, DefaultGrace, h.clock.AwaitTimer(t))
		h.connect(spec, 0)
		h.awaitGrace(spec, 0, false)
		h.clock.Step(DefaultGrace)
		time.Sleep(pending)
		_, ok := h.ended(spec)
		require.False(t, ok, "the session must stay open")
		for _, m := range members {
			require.NoError(t, m.Err())
		}
	})

	t.Run("a member that does not reconnect fails the session", func(t *testing.T) {
		h := newHarness(t)
		spec := h.ensureForGrace(2)
		members := h.connectAll(spec)
		members[0].nc.Close()
		require.Equal(t, DefaultGrace, h.clock.AwaitTimer(t))
		h.clock.Step(DefaultGrace - time.Millisecond)
		time.Sleep(pending)
		require.NoError(t, members[1].Err(), "the grace period has not run out yet")
		h.clock.Step(time.Millisecond)
		require.ErrorIs(t, awaitEnd(t, members[1]), &session.AbortError{Reason: session.ReasonMemberFailed})
		require.Eventually(t, func() bool { return len(h.observer.of(spec.UID)) == 2 }, 5*time.Second,
			5*time.Millisecond)
		assert.Equal(t, "failed "+string(spec.UID)+" MemberFailed", h.observer.of(spec.UID)[1])
	})

	t.Run("a member that left still gets a grace period", func(t *testing.T) {
		h := newHarness(t)
		spec := h.ensureForGrace(2)
		members := h.connectAll(spec)
		require.NoError(t, members[0].leave(within(t)))
		members[0].nc.Close()
		require.Equal(t, DefaultGrace, h.clock.AwaitTimer(t))
		h.clock.Step(DefaultGrace)
		require.ErrorIs(t, awaitEnd(t, members[1]), &session.AbortError{Reason: session.ReasonMemberFailed})
	})

	t.Run("a session that ended starts none", func(t *testing.T) {
		h := newHarness(t)
		spec := h.ensureForGrace(2)
		members := h.connectAll(spec)
		require.NoError(t, members[0].fail(within(t), "Refused"))
		for _, m := range members {
			require.ErrorIs(t, awaitEnd(t, m), &session.AbortError{Reason: "Refused"})
		}
		h.requireNoGrace(spec)
	})

	t.Run("a member keeps its session while one of its connections is open", func(t *testing.T) {
		h := newHarness(t)
		spec := h.ensureForGrace(1)
		m := h.connect(spec, 0)
		extra, _ := h.raw(spec, 0)
		extra.Close()
		h.requireNoGrace(spec)
		require.NoError(t, m.Err())
	})

	t.Run("a refused client cannot pose as a member", func(t *testing.T) {
		h := newHarness(t)
		spec := h.ensureForGrace(1)
		h.connect(spec, 0)
		_, err := h.dial(spec, spec.Members[0].ID, "stolen", 0)
		require.ErrorIs(t, err, nats.ErrAuthorization)
		h.requireNoGrace(spec)
	})
}
