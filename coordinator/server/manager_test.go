// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
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
		{"a UID too long for its buckets", Spec{UID: types.UID(strings.Repeat("u", session.MaxAttemptLength+1)),
			Members: members("a"), Deadline: deadline}},
		{"no members", Spec{UID: "u", Deadline: deadline}},
		{"no deadline", Spec{UID: "u", Members: members("a")}},
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
			require.ErrorIs(t, tt.spec.validate(), session.ErrInvalid)
		})
	}
	require.NoError(t, Spec{UID: "u", Members: []session.Member{{ID: "b", Rank: 1}, {ID: "a", Rank: 0}},
		Deadline: deadline}.validate(), "members may come in any order")
}

// TestEnsure covers creating a session, and ensuring it again.
func TestEnsure(t *testing.T) {
	h := newHarness(t)

	t.Run("creates both buckets in memory, and reports the session", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(1)
		for _, bucket := range []string{session.SnapshotBucket(string(spec.UID)),
			session.LibraryBucket(string(spec.UID))} {
			stream, err := h.srv.js.Stream(within(t), "KV_"+bucket)
			require.NoError(t, err)
			// Sessions never reach the disk.
			assert.Equal(t, jetstream.MemoryStorage, stream.CachedInfo().Config.Storage, "bucket %s", bucket)
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
		require.ErrorIs(t, h.srv.Ensure(within(t), later), session.ErrInvalid)
		renamed := spec
		renamed.Members = []session.Member{{ID: "member-0", Rank: 0}, {ID: "other", Rank: 1}}
		require.ErrorIs(t, h.srv.Ensure(within(t), renamed), session.ErrInvalid)
	})

	t.Run("the longest attempt UID", func(t *testing.T) {
		h := h.sub(t)
		spec := h.spec(1)
		spec.UID = types.UID(strings.Repeat("u", session.MaxAttemptLength))
		require.NoError(t, h.srv.Ensure(within(t), spec))
	})
}

// TestEndAtTheMemoryCap covers a session that every member left, or whose
// deadline or grace period runs out, or that the host removes, while another
// session fills the memory cap: the control keys are in file storage, so the
// end is recorded, and every member learns it.
func TestEndAtTheMemoryCap(t *testing.T) {
	capped := func(cfg *Config) { cfg.MaxBytes, cfg.MaxValue = 2<<20, 64<<10 }
	// atTheCap returns a session with n members, connected, and a session of
	// another attempt that has filled the memory cap.
	atTheCap := func(t *testing.T, h *harness, n int) (Spec, []*member) {
		t.Helper()
		spec := h.ensure(n)
		members := h.connectAll(spec)
		filler := h.spec(1)
		filler.Deadline = spec.Deadline.Add(time.Hour)
		require.NoError(t, h.srv.Ensure(within(t), filler))
		h.fillCap(h.connect(filler, 0))
		return spec, members
	}

	t.Run("a session opens", func(t *testing.T) {
		h := newHarness(t, capped)
		h.fillCap(h.connect(h.ensure(1), 0))
		spec := h.spec(1)
		require.NoError(t, h.srv.Ensure(within(t), spec))
		h.connect(spec, 0)
	})

	t.Run("a deadline that passes fails the session", func(t *testing.T) {
		h := newHarness(t, capped)
		spec, members := atTheCap(t, h, 1)
		h.clock.Step(time.Hour)
		endsSoon(t, members[0], &session.AbortError{Reason: session.ReasonDeadlineExceeded})
		reason, ok := h.ended(spec)
		require.True(t, ok)
		assert.Equal(t, session.ReasonDeadlineExceeded, reason)
	})

	t.Run("a grace period that runs out fails the session", func(t *testing.T) {
		h := newHarness(t, capped)
		_, members := atTheCap(t, h, 2)
		members[0].nc.Close()
		h.clock.Await(t, DefaultGrace)
		h.clock.Step(DefaultGrace)
		endsSoon(t, members[1], &session.AbortError{Reason: session.ReasonMemberFailed})
	})

	t.Run("a session every member left closes", func(t *testing.T) {
		h := newHarness(t, capped)
		spec, members := atTheCap(t, h, 2)
		for _, m := range members {
			require.NoError(t, m.leave(within(t)))
		}
		endsSoon(t, members[0], session.ErrClosed)
		require.Eventually(t, func() bool { return len(h.observer.of(spec.UID)) == 2 }, 5*time.Second,
			5*time.Millisecond)
		assert.Equal(t, "closed "+string(spec.UID), h.observer.of(spec.UID)[1])
	})

	t.Run("a member fails the session", func(t *testing.T) {
		h := newHarness(t, capped)
		_, members := atTheCap(t, h, 2)
		require.NoError(t, members[0].fail(within(t), "Refused"))
		endsSoon(t, members[1], &session.AbortError{Reason: "Refused"})
	})

	t.Run("removing the session records Cancelled", func(t *testing.T) {
		h := newHarness(t, capped)
		spec, members := atTheCap(t, h, 1)
		require.NoError(t, h.srv.Remove(within(t), spec.UID))
		endsSoon(t, members[0], &session.AbortError{Reason: session.ReasonCancelled})
	})

	t.Run("shutting down fails every session with CoordinatorLost", func(t *testing.T) {
		h := newHarness(t, capped)
		_, members := atTheCap(t, h, 2)
		require.NoError(t, returns(t, "Shutdown", func() error { return h.srv.Shutdown(within(t)) }))
		for _, m := range members {
			endsSoon(t, m, &session.AbortError{Reason: session.ReasonCoordinatorLost})
		}
	})
}

// TestEndAtAFullControlBucket covers a session whose own members fill its
// control bucket, which only they can: no write fits, not even its end. The
// coordinator keeps trying to record the end, stops once the session is
// removed or the coordinator shuts down, and the host removes the session once
// its deadline has passed.
func TestEndAtAFullControlBucket(t *testing.T) {
	t.Run("a session is removed past its deadline, which stops the retries", func(t *testing.T) {
		h := newHarness(t)
		ctx := within(t)
		spec := h.ensure(1)
		h.fillControl(h.connect(spec, 0))
		h.clock.Step(time.Hour)
		h.awaitRetry(spec)
		_, ok := h.ended(spec)
		require.False(t, ok, "the bucket must refuse the end")
		require.NoError(t, h.srv.Remove(ctx, spec.UID))
		require.NoError(t, returns(t, "Shutdown", func() error { return h.srv.Shutdown(within(t)) }))
	})

	t.Run("a full bucket does not reach another session", func(t *testing.T) {
		h := newHarness(t)
		spec, other := h.ensure(1), h.ensure(1)
		h.fillControl(h.connect(spec, 0))
		o := h.connect(other, 0)
		require.NoError(t, o.arrive(within(t), "admitted"))
		require.NoError(t, o.fail(within(t), "Refused"))
		// Shutdown could not record the full session's end.
		require.NoError(t, h.srv.Remove(within(t), spec.UID))
	})

	t.Run("shutting down stops the retries", func(t *testing.T) {
		h := newHarness(t)
		spec := h.ensure(1)
		h.fillControl(h.connect(spec, 0))
		h.clock.Step(time.Hour)
		h.awaitRetry(spec)
		require.Error(t, returns(t, "Shutdown", func() error { return h.srv.Shutdown(within(t)) }),
			"the bucket refuses CoordinatorLost too")
	})
}

// fillControl writes arrivals through m until its session's control bucket
// refuses one, which leaves no room to record the session's end either.
func (h *harness) fillControl(m *member) {
	h.t.Helper()
	ctx := within(h.t)
	var err error
	for i := 0; err == nil; i++ {
		err = m.arrive(ctx, fmt.Sprintf("barrier-%d", i))
	}
	requireCode(h.t, err, codeStoreFailed)
}

// fillCap fills the memory cap through m until not even an empty value fits.
func (h *harness) fillCap(m *member) {
	h.t.Helper()
	ctx := within(h.t)
	for pass, size := range []int{60 << 10, 1 << 10, 32, 0} {
		var err error
		for i := 0; err == nil; i++ {
			err = m.insert(ctx, session.NamespaceSnapshot, fmt.Sprintf("%c%d", 'a'+pass, i), make([]byte, size))
		}
		requireCode(h.t, err, codeInsufficientResources)
	}
}

// awaitRetry waits until the coordinator has failed to record the end of
// spec's session, and is trying again.
func (h *harness) awaitRetry(spec Spec) {
	h.t.Helper()
	h.awaitLog(spec, "retrying until its end is recorded", 10*time.Second)
}

// awaitLog waits, for at most wait, until the coordinator has logged a line
// about spec's session that holds text.
func (h *harness) awaitLog(spec Spec, text string, wait time.Duration) {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		return slices.ContainsFunc(h.logs.all(), func(line string) bool {
			return strings.Contains(line, text) && strings.Contains(line, string(spec.UID))
		})
	}, wait, 5*time.Millisecond, "the coordinator did not log %q", text)
}

// TestWatchStopsOnItsOwn covers the coordinator's watch of a session stopping
// while the session is open. A member deletes the watch's consumer, which its
// permissions allow on its own buckets, and takes the freed slot, so the
// bucket is at its consumer limit when nats.go, missing the consumer's
// heartbeats, recreates it, and nats.go stops the watch. The deadline still
// fails the session, and once a slot frees, the coordinator watches again and
// sees the end, which ends the session's life. nats.go misses two heartbeats
// first, so the test waits about ten seconds.
func TestWatchStopsOnItsOwn(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec := h.ensure(1)
	h.clock.Await(t, time.Hour)
	m := h.connect(spec, 0)
	stream := "KV_" + m.control.Bucket()
	str, err := h.srv.js.Stream(ctx, stream)
	require.NoError(t, err)
	var watched string
	list := str.ListConsumers(ctx)
	for c := range list.Info() {
		if !strings.HasPrefix(c.Config.DeliverSubject, session.InboxPrefix(string(spec.UID), m.id)+".") {
			watched = c.Name
		}
	}
	require.NoError(t, list.Err())
	require.NotEmpty(t, watched, "the coordinator must watch the session")

	// The member holds every consumer its share allows, its watch of the end
	// included, then deletes the coordinator's consumer and takes its slot.
	life, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hold := func(ctx context.Context) {
		_, err := m.control.Watch(ctx, "barrier.x.*")
		require.NoError(t, err)
	}
	for range session.MaxConsumersPerMember - 1 {
		hold(life)
	}
	reply, err := m.nc.Request("$JS.API.CONSUMER.DELETE."+stream+"."+watched, nil, 2*time.Second)
	require.NoError(t, err)
	require.Contains(t, string(reply.Data), `"success":true`)
	last, free := context.WithCancel(life)
	hold(last)

	h.awaitLog(spec, "The session's watch stopped; watching again", 30*time.Second)
	require.Eventually(t, func() bool { return slicesContain(h.logs.all(), "recreating ordered consumer") },
		5*time.Second, 5*time.Millisecond, "nats.go's error must reach the coordinator's log")
	h.clock.Step(time.Hour)
	require.ErrorIs(t, awaitEnd(t, m), &session.AbortError{Reason: session.ReasonDeadlineExceeded})
	reason, ok := h.ended(spec)
	require.True(t, ok)
	assert.Equal(t, session.ReasonDeadlineExceeded, reason)

	a := h.srv.lookup(spec.UID)
	require.NoError(t, a.life.Err(), "the session's life must outlast its watch")
	free()
	select {
	case <-a.life.Done():
	case <-ctx.Done():
		t.Fatal("the coordinator did not watch the session again once a slot freed")
	}
}

// endsSoon checks that m sees its session end as want says within twice the
// longest wait between tries to record the end.
func endsSoon(t *testing.T, m *member, want error) {
	t.Helper()
	select {
	case <-m.done:
		require.ErrorIs(t, m.err, want)
	case <-time.After(2 * maxEndRetry):
		t.Fatalf("the session did not end with %v within %v", want, 2*maxEndRetry)
	}
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
	kv, err := js.KeyValue(ctx, session.ControlBucket(string(spec.UID)))
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
// and its buckets go. What a member makes of that is sessiontest's to test: a
// member may find the buckets gone before it receives the end.
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

	t.Run("an unknown attempt", func(t *testing.T) {
		require.NoError(t, h.srv.Remove(ctx, "unknown"))
	})
}

// TestEnsureCancelledWhileWatching is a stress test. It ends Ensure's context
// while follow may still be creating the session's watch: as soon as the
// session's Snapshot bucket reports the watch's consumer. nats.go may never
// close the channel of a watch cancelled as it is created
// (nats-io/nats.go#2223), so an Ensure that waited for that channel would
// never return, and, since Ensure holds control, neither would Remove or
// Shutdown. Nothing makes the cancellation land in that window, which only a
// hook in production code could hold open, so each Ensure gives the race one
// chance and the test ensures many attempts. The window is narrow: before the
// fix, the test hung in 1 of 8 concurrent runs of 100 under -race.
func TestEnsureCancelledWhileWatching(t *testing.T) {
	h := newHarness(t)
	for i := range 100 {
		spec := h.spec(1)
		ctx, cancel := context.WithCancel(context.Background())
		watched, err := h.srv.conn.Subscribe(fmt.Sprintf("%s.KV_%s.*", natsserver.JSAdvisoryConsumerCreatedPre,
			session.SnapshotBucket(string(spec.UID))), func(*nats.Msg) { cancel() })
		require.NoError(t, err)
		err = returns(t, fmt.Sprintf("Ensure %d", i), func() error { return h.srv.Ensure(ctx, spec) })
		cancel()
		require.NoError(t, watched.Unsubscribe())
		if err != nil {
			require.ErrorIs(t, err, context.Canceled, "Ensure %d", i)
		}
		require.NoError(t, returns(t, fmt.Sprintf("Remove %d", i), func() error {
			return h.srv.Remove(within(t), spec.UID)
		}))
	}
	require.NoError(t, returns(t, "Shutdown", func() error { return h.srv.Shutdown(within(t)) }))
}

// returns calls call, and fails the test unless it returns within 20 seconds.
func returns(t *testing.T, what string, call func() error) error {
	t.Helper()
	returned := make(chan error, 1)
	go func() { returned <- call() }()
	select {
	case err := <-returned:
		return err
	case <-within(t).Done():
		t.Fatalf("%s did not return", what)
		return nil
	}
}

// TestBucketStorage checks where a session's buckets live: the control bucket
// in file storage, bounded by the membership, and the data buckets in memory.
func TestBucketStorage(t *testing.T) {
	h := newHarness(t)
	spec := h.ensure(3)
	for bucket, want := range map[string]struct {
		storage  jetstream.StorageType
		maxBytes int64
	}{
		session.ControlBucket(string(spec.UID)):  {jetstream.FileStorage, session.ControlBytes(3)},
		session.SnapshotBucket(string(spec.UID)): {jetstream.MemoryStorage, -1},
		session.LibraryBucket(string(spec.UID)):  {jetstream.MemoryStorage, -1},
	} {
		stream, err := h.srv.js.Stream(within(t), "KV_"+bucket)
		require.NoError(t, err)
		cfg := stream.CachedInfo().Config
		assert.Equal(t, want.storage, cfg.Storage, bucket)
		assert.Equal(t, want.maxBytes, cfg.MaxBytes, bucket)
	}
}
