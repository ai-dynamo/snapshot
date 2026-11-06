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
// deadline or grace period runs out, while the memory cap refuses the write
// that records its end: the coordinator keeps trying, so the session ends once
// memory frees, and stops once the session is removed or the coordinator shuts
// down. A session whose own keys fill the cap never ends, and the host removes
// it once its deadline has passed, which frees the cap.
func TestEndAtTheMemoryCap(t *testing.T) {
	capped := func(cfg *Config) { cfg.MaxBytes, cfg.MaxValue = 2<<20, 64<<10 }

	t.Run("a deadline that passes at the cap fails the session once memory frees", func(t *testing.T) {
		h := newHarness(t, capped)
		ctx := within(t)
		spec := h.ensure(1)
		m := h.connect(spec, 0)
		filler := h.spec(1)
		filler.Deadline = spec.Deadline.Add(time.Hour)
		require.NoError(t, h.srv.Ensure(ctx, filler))
		h.fillCap(h.connect(filler, 0))
		h.clock.Step(time.Hour)
		h.awaitRetry(spec)
		_, ok := h.ended(spec)
		require.False(t, ok, "the cap must refuse the end")
		require.NoError(t, h.srv.Remove(ctx, filler.UID))
		endsSoon(t, m, &session.AbortError{Reason: session.ReasonDeadlineExceeded})
		require.Eventually(t, func() bool { return len(h.observer.of(spec.UID)) == 2 }, 5*time.Second,
			5*time.Millisecond)
		assert.Equal(t, "failed "+string(spec.UID)+" DeadlineExceeded", h.observer.of(spec.UID)[1])
	})

	t.Run("a grace period that runs out at the cap fails the session once memory frees", func(t *testing.T) {
		h := newHarness(t, capped)
		spec := h.ensure(2)
		members := h.connectAll(spec)
		filler := h.ensure(1)
		h.fillCap(h.connect(filler, 0))
		members[0].nc.Close()
		h.clock.Await(t, DefaultGrace)
		h.clock.Step(DefaultGrace)
		h.awaitRetry(spec)
		_, ok := h.ended(spec)
		require.False(t, ok, "the cap must refuse the end")
		back := h.connect(spec, 0)
		require.NoError(t, h.srv.Remove(within(t), filler.UID))
		endsSoon(t, members[1], &session.AbortError{Reason: session.ReasonMemberFailed})
		endsSoon(t, back, &session.AbortError{Reason: session.ReasonMemberFailed})
	})

	t.Run("a session every member left at the cap closes once memory frees", func(t *testing.T) {
		h := newHarness(t, capped)
		spec := h.ensure(1)
		m := h.connect(spec, 0)
		filler := h.spec(1)
		filler.Deadline = spec.Deadline.Add(time.Hour)
		require.NoError(t, h.srv.Ensure(within(t), filler))
		// The member's departure takes the session past the cap, which then
		// refuses the coordinator's write that closes it.
		h.fillTo(h.connect(filler, 0), h.srv.cfg.MaxBytes)
		require.NoError(t, m.leave(within(t)))
		h.awaitRetry(spec)
		_, ok := h.ended(spec)
		require.False(t, ok, "the cap must refuse the end")
		// The deadline that passes meanwhile does not fail a session that is
		// closing.
		h.clock.Step(time.Hour)
		require.NoError(t, h.srv.Remove(within(t), filler.UID))
		endsSoon(t, m, session.ErrClosed)
		require.Eventually(t, func() bool { return len(h.observer.of(spec.UID)) == 2 }, 5*time.Second,
			5*time.Millisecond)
		assert.Equal(t, "closed "+string(spec.UID), h.observer.of(spec.UID)[1])
	})

	t.Run("a session whose own keys fill the cap is removed past its deadline, which frees the cap", func(t *testing.T) {
		h := newHarness(t, capped)
		ctx := within(t)
		spec, other := h.ensure(1), h.spec(1)
		other.Deadline = spec.Deadline.Add(time.Hour)
		require.NoError(t, h.srv.Ensure(ctx, other))
		o := h.connect(other, 0)
		h.fillCap(h.connect(spec, 0))
		h.clock.Step(time.Hour)
		h.awaitRetry(spec)
		requireCode(t, o.insert(ctx, session.NamespaceSnapshot, "k", nil), codeInsufficientResources)
		require.NoError(t, h.srv.Remove(ctx, spec.UID))
		require.NoError(t, o.insert(ctx, session.NamespaceSnapshot, "k", nil), "removing the session frees the cap")
		// Removing the session stops the retries, which Shutdown waits for.
		require.NoError(t, returns(t, "Shutdown", func() error { return h.srv.Shutdown(within(t)) }))
	})

	t.Run("shutting down stops the retries", func(t *testing.T) {
		h := newHarness(t, capped)
		spec := h.ensure(1)
		h.fillCap(h.connect(spec, 0))
		h.clock.Step(time.Hour)
		h.awaitRetry(spec)
		require.Error(t, returns(t, "Shutdown", func() error { return h.srv.Shutdown(within(t)) }),
			"the cap refuses CoordinatorLost too")
	})
}

// fillCap fills the memory cap through m until not even an empty value fits,
// which leaves no room to record any session's end either.
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

// fillTo writes through m until the memory every session holds together is
// used bytes. The cap refuses a write only once that memory is past it, so the
// next write fits whatever its size, and no other does. Each write takes its
// subject, its headers, which a Put has none of, and its value, and 16 bytes
// more.
func (h *harness) fillTo(m *member, used int64) {
	h.t.Helper()
	ctx := within(h.t)
	for i := 0; ; i++ {
		key := fmt.Sprintf("snapshot.f%d", i)
		room := used - int64(h.srv.account.JetStreamUsage().Memory)
		size := min(room-int64(len("$KV."+m.snapshot.Bucket()+"."+key))-16, int64(h.srv.cfg.MaxValue))
		require.GreaterOrEqual(h.t, size, int64(0), "the memory in use must stop short of %d bytes", used)
		_, err := m.snapshot.Put(ctx, key, make([]byte, size))
		require.NoError(h.t, err)
		if int64(h.srv.account.JetStreamUsage().Memory) == used {
			return
		}
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
	stream := "KV_" + m.snapshot.Bucket()
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
		_, err := m.snapshot.Watch(ctx, "snapshot.x")
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

// TestCancel covers cancelling a session, as the host does once its group
// resource's deletion starts: the session fails with Cancelled unless every
// member has arrived at the admitted barrier, and a session that ended keeps
// its end. What a member makes of it is sessiontest's to test.
func TestCancel(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)

	t.Run("a session not every member has arrived in fails", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(3)
		members := h.connectAll(spec)
		require.NoError(t, members[0].arrive(ctx, session.BarrierAdmitted))
		require.NoError(t, members[1].arrive(ctx, "other"), "another barrier does not count")
		require.NoError(t, members[2].arrive(ctx, session.BarrierAdmitted))
		require.NoError(t, members[2].snapshot.Delete(ctx, session.BarrierKey(session.BarrierAdmitted, members[2].id)),
			"an arrival deleted does not count")
		require.NoError(t, h.srv.Cancel(ctx, spec.UID))
		for _, m := range members {
			require.ErrorIs(t, awaitEnd(t, m), &session.AbortError{Reason: session.ReasonCancelled})
		}
		value, ok := h.ended(spec)
		require.True(t, ok)
		assert.Equal(t, session.ReasonCancelled, value)
	})

	t.Run("a session every member has arrived in stays open", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(2)
		members := h.connectAll(spec)
		for _, m := range members {
			require.NoError(t, m.arrive(ctx, session.BarrierAdmitted))
		}
		require.NoError(t, h.srv.Cancel(ctx, spec.UID))
		_, ok := h.ended(spec)
		assert.False(t, ok)
		assert.Equal(t, []string{"opened " + string(spec.UID)}, h.observer.of(spec.UID))
	})

	t.Run("a session that ended keeps its end", func(t *testing.T) {
		h := h.sub(t)
		spec := h.ensure(1)
		m := h.connect(spec, 0)
		require.NoError(t, m.fail(ctx, "Refused"))
		require.NoError(t, h.srv.Cancel(ctx, spec.UID))
		value, _ := h.ended(spec)
		assert.Equal(t, "Refused", value)
	})

	t.Run("an unknown attempt", func(t *testing.T) {
		require.ErrorIs(t, h.srv.Cancel(ctx, "unknown"), session.ErrNotFound)
	})
}

// TestEnded covers what the host reads of a session: whether the coordinator
// holds it, from Ensure until Remove, and whether it has ended, however it
// ended.
func TestEnded(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	requireEnded := func(t *testing.T, uid types.UID, wantEnded, wantHeld bool) {
		t.Helper()
		ended, held := h.srv.Ended(uid)
		assert.Equal(t, wantEnded, ended, "ended")
		assert.Equal(t, wantHeld, held, "held")
	}

	t.Run("from Ensure until Remove", func(t *testing.T) {
		spec := h.spec(1)
		requireEnded(t, spec.UID, false, false)
		require.NoError(t, h.srv.Ensure(ctx, spec))
		requireEnded(t, spec.UID, false, true)
		require.NoError(t, h.srv.Fail(ctx, spec.UID, "Refused"))
		requireEnded(t, spec.UID, true, true)
		require.NoError(t, h.srv.Remove(ctx, spec.UID))
		requireEnded(t, spec.UID, false, false)
	})

	t.Run("a member's end, once the coordinator has seen it", func(t *testing.T) {
		spec := h.ensure(2)
		members := h.connectAll(spec)
		for _, m := range members {
			require.NoError(t, m.leave(ctx))
		}
		require.Eventually(t, func() bool {
			ended, _ := h.srv.Ended(spec.UID)
			return ended
		}, 5*time.Second, 5*time.Millisecond)
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
