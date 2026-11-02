// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestSpecValidation covers the specs Ensure refuses.
func TestSpecValidation(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	members := func(ids ...string) []session.Member {
		var m []session.Member
		for rank, id := range ids {
			m = append(m, session.Member{ID: id, Rank: rank})
		}
		return m
	}
	tests := []struct {
		name string
		spec Spec
	}{
		{"no UID", Spec{Members: members("a"), Deadline: deadline}},
		{"a UID that is not one token", Spec{UID: "a.b", Members: members("a"), Deadline: deadline}},
		{"no members", Spec{UID: "u", Deadline: deadline}},
		{"no deadline", Spec{UID: "u", Members: members("a")}},
		{"a member without an ID", Spec{UID: "u", Members: members(""), Deadline: deadline}},
		{"a member ID with a dot", Spec{UID: "u", Members: members("pod.a"), Deadline: deadline}},
		{"a member named twice", Spec{UID: "u", Members: members("a", "a"), Deadline: deadline}},
		{"a rank outside the set", Spec{UID: "u", Members: []session.Member{{ID: "a", Rank: 1}},
			Deadline: deadline}},
		{"a rank taken twice", Spec{UID: "u", Members: []session.Member{{ID: "a"}, {ID: "b"}},
			Deadline: deadline}},
		{"a negative rank", Spec{UID: "u", Members: []session.Member{{ID: "a", Rank: -1}},
			Deadline: deadline}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.spec.validate(), ErrInvalid)
		})
	}
	require.NoError(t, Spec{UID: "u", Members: []session.Member{{ID: "b", Rank: 1}, {ID: "a", Rank: 0}},
		Deadline: deadline}.validate(), "members may come in any order")
}

// TestEnsure covers creating a session, and ensuring it again.
func TestEnsure(t *testing.T) {
	h := newHarness(t)

	t.Run("creates both buckets and meta", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(3)
		meta := h.connect(spec, 2).meta
		assert.Equal(t, spec.byRank(), meta.Members)
		assert.True(t, spec.Deadline.Equal(meta.Deadline))
		assert.NotZero(t, meta.Epoch)
		assert.Equal(t, session.MaxValue, meta.MaxValue)
		assert.Equal(t, session.MaxLibraryValue, meta.MaxLibraryValue)
		for bucket, limits := range map[string][2]int{
			session.SnapshotBucket(string(spec.UID)): {-1, session.MaxValue},
			session.LibraryBucket(string(spec.UID)):  {session.MaxLibraryKeys, session.MaxLibraryValue},
		} {
			stream, err := h.srv.js.Stream(within(t), "KV_"+bucket)
			require.NoError(t, err)
			cfg := stream.CachedInfo().Config
			assert.Equal(t, int64(-1), cfg.MaxBytes, "bucket %s must reserve no memory", bucket)
			assert.Equal(t, int64(limits[0]), cfg.MaxMsgs, "bucket %s", bucket)
			assert.Equal(t, int32(limits[1]+valueHeadroom), cfg.MaxMsgSize, "bucket %s", bucket)
			assert.Equal(t, jetstream.MemoryStorage, cfg.Storage, "bucket %s", bucket)
			assert.Equal(t, 1, cfg.Replicas, "bucket %s", bucket)
		}
		assert.Equal(t, []string{"opened " + string(spec.UID)}, h.observer.of(spec.UID))
	})

	t.Run("ensuring the same spec again does nothing", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(2)
		epoch := h.srv.lookup(spec.UID).epoch
		reordered := spec
		reordered.Members = []session.Member{spec.Members[1], spec.Members[0]}
		require.NoError(t, h.srv.Ensure(within(t), reordered))
		assert.Equal(t, epoch, h.srv.lookup(spec.UID).epoch)
		assert.Len(t, h.observer.of(spec.UID), 1)
	})

	t.Run("a different spec is refused", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(2)
		later := spec
		later.Deadline = spec.Deadline.Add(time.Minute)
		require.ErrorIs(t, h.srv.Ensure(within(t), later), ErrInvalid)
		renamed := spec
		renamed.Members = []session.Member{{ID: "member-0", Rank: 0}, {ID: "other", Rank: 1}}
		require.ErrorIs(t, h.srv.Ensure(within(t), renamed), ErrInvalid)
	})

	t.Run("a session past its deadline starts failed", func(t *testing.T) {
		h := h.sub(t)
		spec := h.spec(1)
		spec.Deadline = h.clock.Now().Add(-time.Second)
		require.NoError(t, h.srv.Ensure(within(t), spec))
		reason, ok := h.ended(spec)
		require.True(t, ok)
		assert.Equal(t, session.ReasonDeadlineExceeded, reason)
		m := h.connect(spec, 0)
		require.ErrorIs(t, awaitEnd(t, m), &session.AbortError{Reason: session.ReasonDeadlineExceeded})
		assert.Equal(t, []string{"opened " + string(spec.UID), "failed " + string(spec.UID) + " DeadlineExceeded"},
			h.observer.of(spec.UID))
	})
}

// TestDeadline covers the deadline failing a session, and not one that ended.
func TestDeadline(t *testing.T) {
	h := newHarness(t)
	spec := h.ensure(2)
	require.Equal(t, time.Hour, h.clock.AwaitTimer(t))
	members := h.connectAll(spec)
	h.clock.Step(time.Hour)
	for _, m := range members {
		require.ErrorIs(t, awaitEnd(t, m), &session.AbortError{Reason: session.ReasonDeadlineExceeded})
	}
	require.Eventually(t, func() bool { return len(h.observer.of(spec.UID)) == 2 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "failed "+string(spec.UID)+" DeadlineExceeded", h.observer.of(spec.UID)[1])
}

// TestClosing covers the session closing once every member has left, and only
// then.
func TestClosing(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec := h.ensure(2)
	members := h.connectAll(spec)
	require.NoError(t, members[0].leave(ctx))
	require.NoError(t, members[0].leave(ctx))
	time.Sleep(pending)
	_, ok := h.ended(spec)
	require.False(t, ok, "one member leaving must not close the session")
	require.NoError(t, members[1].leave(ctx))
	for _, m := range members {
		require.ErrorIs(t, awaitEnd(t, m), session.ErrClosed)
	}
	reason, ok := h.ended(spec)
	require.True(t, ok)
	assert.Empty(t, reason)
	require.Eventually(t, func() bool { return len(h.observer.of(spec.UID)) == 2 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "closed "+string(spec.UID), h.observer.of(spec.UID)[1])
}

// TestFail covers failing a session on the coordinator's own authority.
func TestFail(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec := h.ensure(2)
	members := h.connectAll(spec)
	require.ErrorIs(t, h.srv.Fail(ctx, spec.UID, "not a reason"), ErrInvalid)
	require.NoError(t, h.srv.Fail(ctx, spec.UID, session.ReasonMemberFailed))
	require.NoError(t, h.srv.Fail(ctx, spec.UID, session.ReasonMemberFailed), "failing again with the same reason")
	require.ErrorIs(t, h.srv.Fail(ctx, spec.UID, "Other"), &session.AbortError{Reason: session.ReasonMemberFailed},
		"the first reason wins")
	for _, m := range members {
		require.ErrorIs(t, awaitEnd(t, m), &session.AbortError{Reason: session.ReasonMemberFailed})
	}
	require.ErrorIs(t, h.srv.Fail(ctx, "unknown", "Refused"), ErrNotFound)
	assert.Equal(t, []string{"opened " + string(spec.UID), "failed " + string(spec.UID) + " MemberFailed"},
		h.observer.of(spec.UID))
}

// TestInvalidReason covers a member that writes the ended key with bytes that
// are no condition reason: the session fails with InvalidReason, and the bytes
// reach neither the Observer nor the log.
func TestInvalidReason(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec := h.ensure(2)
	m := h.connect(spec, 1)
	nc, _ := h.raw(spec, 0)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	kv, err := js.KeyValue(ctx, session.SnapshotBucket(string(spec.UID)))
	require.NoError(t, err)
	_, err = kv.Create(ctx, session.KeyEnded, []byte("not a reason: secret\x00"))
	require.NoError(t, err)

	require.ErrorIs(t, awaitEnd(t, m), &session.AbortError{Reason: session.ReasonInvalid})
	require.Eventually(t, func() bool { return len(h.observer.of(spec.UID)) == 2 }, 5*time.Second,
		5*time.Millisecond)
	assert.Equal(t, "failed "+string(spec.UID)+" "+session.ReasonInvalid, h.observer.of(spec.UID)[1])
	for _, line := range h.logs.all() {
		assert.NotContains(t, line, "secret")
	}
}

// TestRemove covers removing a session: one still open fails with Cancelled,
// and its buckets go. What a member makes of that is the session client's to
// test: a member may find the buckets gone before it receives the end.
func TestRemove(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)

	t.Run("an open session is cancelled, then deleted", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(2)
		require.NoError(t, h.srv.Remove(ctx, spec.UID))
		require.Eventually(t, func() bool { return len(h.observer.of(spec.UID)) == 2 }, 5*time.Second,
			5*time.Millisecond)
		assert.Equal(t, "failed "+string(spec.UID)+" "+session.ReasonCancelled, h.observer.of(spec.UID)[1])
		for _, bucket := range []string{session.SnapshotBucket(string(spec.UID)),
			session.LibraryBucket(string(spec.UID))} {
			_, err := h.srv.js.KeyValue(ctx, bucket)
			require.ErrorIs(t, err, jetstream.ErrBucketNotFound)
		}
		require.Nil(t, h.srv.lookup(spec.UID))
		require.NoError(t, h.srv.Remove(ctx, spec.UID), "removing twice")
	})

	t.Run("an ended session keeps its outcome", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(1)
		m := h.connect(spec, 0)
		require.NoError(t, m.fail(ctx, "Refused"))
		require.NoError(t, h.srv.Remove(ctx, spec.UID))
		require.Eventually(t, func() bool { return len(h.observer.of(spec.UID)) == 2 }, 5*time.Second,
			5*time.Millisecond)
		assert.Equal(t, []string{"opened " + string(spec.UID), "failed " + string(spec.UID) + " Refused"},
			h.observer.of(spec.UID))
	})

	t.Run("an unknown attempt", func(t *testing.T) {
		require.NoError(t, h.srv.Remove(ctx, "unknown"))
	})
}

// TestShutdown covers shutting down: every open session fails with
// CoordinatorLost, and the server refuses everything afterwards.
func TestShutdown(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	h := startHarness(t)
	h.srv.MarkReady()
	ctx := within(t)
	open, done := h.ensure(2), h.ensure(1)
	members := h.connectAll(open)
	finished := h.connect(done, 0)
	require.NoError(t, finished.fail(ctx, "Refused"))
	require.ErrorIs(t, awaitEnd(t, finished), &session.AbortError{Reason: "Refused"})

	addr := h.srv.Addr().String()
	require.NoError(t, h.srv.Shutdown(ctx))
	for _, m := range members {
		require.ErrorIs(t, awaitEnd(t, m), session.ErrCoordinatorLost)
	}
	assert.Equal(t, "failed "+string(open.UID)+" CoordinatorLost", h.observer.of(open.UID)[1])
	assert.Len(t, h.observer.of(done.UID), 2, "an ended session is not failed again")
	require.Error(t, h.srv.Ensure(ctx, h.spec(1)))
	require.Error(t, h.srv.Remove(ctx, open.UID))
	require.NoError(t, h.srv.Shutdown(ctx), "shutting down twice")
	_, err := net.DialTimeout("tcp", addr, time.Second)
	require.Error(t, err, "the listener must be closed")
}

// TestRestart covers a coordinator that restarts under a session: the next
// one refuses members until its reconciler has recreated the session, then
// fails it as soon as a member that joined the lost one reconnects with the
// epoch it read, so the members that had not connected yet learn it too.
func TestRestart(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	first := startHarness(t)
	first.srv.MarkReady()
	spec := first.ensure(2)
	epoch := first.connect(spec, 0).meta.Epoch
	port := first.srv.Addr().(*net.TCPAddr).Port

	// A crash: the server goes away without failing anything.
	first.srv.mu.Lock()
	first.srv.shutdown = true
	first.srv.mu.Unlock()
	first.srv.stop()

	second := startHarness(t, func(cfg *Config) { cfg.Port, cfg.TLS = port, first.cfg.TLS })
	second.ca = first.ca
	_, err := second.dial(spec, spec.Members[0].ID, agentToken, epoch)
	require.ErrorIs(t, err, nats.ErrAuthorization, "the coordinator must refuse members until it is ready")

	require.NoError(t, second.srv.Ensure(within(t), spec))
	second.srv.MarkReady()
	rejoined, err := second.dial(spec, spec.Members[0].ID, agentToken, epoch)
	require.NoError(t, err)
	defer rejoined.Close()
	late := second.connect(spec, 1)
	require.ErrorIs(t, awaitEnd(t, late), session.ErrCoordinatorLost)
	reason, ok := second.ended(spec)
	require.True(t, ok)
	assert.Equal(t, session.ReasonCoordinatorLost, reason)
	assert.Equal(t, []string{"opened " + string(spec.UID), "failed " + string(spec.UID) + " CoordinatorLost"},
		second.observer.of(spec.UID))
}
