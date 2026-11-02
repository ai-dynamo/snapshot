// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPermissionsAllowEveryPattern makes every request of every session
// pattern, in every namespace, over members' connections under the permissions
// the coordinator grants: binding both buckets and reading meta, which connect
// does, then inserting, reading a key directly and through a watch, deleting
// it, arriving at a barrier and watching it release, leaving, and failing the
// session, which each member's watch of the ended key sees.
func TestPermissionsAllowEveryPattern(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec := h.ensure(2)
	members := h.connectAll(spec)
	for _, ns := range []session.Namespace{session.NamespaceSnapshot, session.NamespaceCuInterpose,
		session.NamespaceLibrary} {
		key := string(ns) + ".k"
		require.NoError(t, members[0].insert(ctx, ns, "k", []byte("v")), "Insert in %s", ns)
		entry, err := members[1].bucket(ns).Get(ctx, key)
		require.NoError(t, err, "Get in %s", ns)
		assert.Equal(t, "v", string(entry.Value()))
		watchUntil(t, members[1].bucket(ns), key, func(present map[string]bool) bool { return present[key] })
		require.NoError(t, members[0].bucket(ns).Delete(ctx, key), "Delete in %s", ns)
	}
	for _, m := range members {
		require.NoError(t, m.arrive(ctx, "admitted"))
	}
	watchUntil(t, members[1].snapshot, session.BarrierKey("admitted", "*"), func(present map[string]bool) bool {
		return len(present) == len(members)
	})
	require.NoError(t, members[0].leave(ctx))
	require.NoError(t, members[1].fail(ctx, "Refused"))
	for _, m := range members {
		require.ErrorIs(t, awaitEnd(t, m), &session.AbortError{Reason: "Refused"})
	}
}

// TestPermissionsRefuse checks every subject a member's connection must not
// reach: another member's keys, meta, another attempt's buckets, the API that
// creates and deletes buckets, other inboxes, and the system account.
func TestPermissionsRefuse(t *testing.T) {
	h := newHarness(t)
	spec, other := h.ensure(2), h.ensure(1)
	nc, violations := h.raw(spec, 0)
	own, theirs := session.SnapshotBucket(string(spec.UID)), session.SnapshotBucket(string(other.UID))
	theirLibrary := session.LibraryBucket(string(other.UID))

	publishes := map[string]string{
		"writing meta":                           "$KV." + own + ".meta",
		"another member's arrival":               "$KV." + own + ".barrier.admitted.member-1",
		"another member's departure":             "$KV." + own + ".left.member-1",
		"a key outside every namespace":          "$KV." + own + ".unscoped",
		"writing another attempt's bucket":       "$KV." + theirs + ".snapshot.k",
		"writing another attempt's library":      "$KV." + theirLibrary + ".library.k",
		"reading another attempt's bucket":       "$JS.API.DIRECT.GET.KV_" + theirs + ".$KV." + theirs + ".meta",
		"binding another attempt's bucket":       "$JS.API.STREAM.INFO.KV_" + theirs,
		"watching another attempt's bucket":      "$JS.API.CONSUMER.CREATE.KV_" + theirs + ".c",
		"listing the account's streams":          "$JS.API.STREAM.LIST",
		"reading the account's JetStream info":   "$JS.API.INFO",
		"creating a bucket":                      "$JS.API.STREAM.CREATE.KV_new",
		"deleting its own bucket":                "$JS.API.STREAM.DELETE.KV_" + own,
		"purging its own bucket":                 "$JS.API.STREAM.PURGE.KV_" + own,
		"updating its own bucket":                "$JS.API.STREAM.UPDATE.KV_" + own,
		"deleting a message of its own bucket":   "$JS.API.STREAM.MSG.DELETE.KV_" + own,
		"reading a message of its own by number": "$JS.API.STREAM.MSG.GET.KV_" + own,
		"pinging the system account":             "$SYS.REQ.SERVER.PING",
	}
	for name, subject := range publishes {
		t.Run(name, func(t *testing.T) {
			_, _ = nc.Request(subject, []byte("{}"), 200*time.Millisecond)
			requireViolation(t, violations, `Publish to "`+subject+`"`)
		})
	}

	subscriptions := map[string]string{
		"another member's inbox":    "_INBOX." + string(spec.UID) + ".member-1.>",
		"another attempt's inbox":   "_INBOX." + string(other.UID) + ".member-0.>",
		"its own bucket's subjects": "$KV." + own + ".>",
		"the system account":        "$SYS.>",
		"everything":                ">",
	}
	for name, subject := range subscriptions {
		t.Run(name, func(t *testing.T) {
			sub, err := nc.SubscribeSync(subject)
			require.NoError(t, err)
			defer func() { _ = sub.Unsubscribe() }()
			require.NoError(t, nc.Flush())
			requireViolation(t, violations, `Subscription to "`+subject+`"`)
		})
	}
}

// TestPermissionsConfineAllowedRequests checks that the API requests a member
// may make reach only its own attempt, whatever they carry.
func TestPermissionsConfineAllowedRequests(t *testing.T) {
	h := newHarness(t)
	spec, other := h.ensure(1), h.ensure(1)
	nc, _ := h.raw(spec, 0)
	own, theirs := session.SnapshotBucket(string(spec.UID)), session.SnapshotBucket(string(other.UID))

	t.Run("a direct get through its own stream finds nothing of another attempt's", func(t *testing.T) {
		msg, err := nc.Request("$JS.API.DIRECT.GET.KV_"+own+".$KV."+theirs+".meta", nil, 2*time.Second)
		require.NoError(t, err)
		assert.Equal(t, "404", msg.Header.Get("Status"))
		assert.Empty(t, msg.Data)
	})

	t.Run("a watch of its own stream delivers nothing of another attempt's", func(t *testing.T) {
		deliver := "_INBOX." + string(spec.UID) + ".member-0.d"
		sub, err := nc.SubscribeSync(deliver)
		require.NoError(t, err)
		defer func() { _ = sub.Unsubscribe() }()
		_, err = nc.Request("$JS.API.CONSUMER.CREATE.KV_"+own+".c",
			[]byte(`{"stream_name":"KV_`+own+`","config":{"name":"c","deliver_subject":"`+deliver+
				`","ack_policy":"none","filter_subject":"$KV.`+theirs+`.>"}}`), 2*time.Second)
		require.NoError(t, err)
		_, err = h.srv.lookup(other.UID).snapshot.Create(within(t), "snapshot.secret", []byte("v"))
		require.NoError(t, err)
		_, err = sub.NextMsg(pending)
		require.ErrorIs(t, err, nats.ErrTimeout, "another attempt's key must not reach this member")
	})
}

// TestDeliveriesCannotChangeAnotherAttemptsKeys guards another attempt's keys
// against a member. JetStream does not bound a push consumer's deliveries by
// its creator's publish permissions (nats-server#8274), so a member may
// deliver its own writes to another attempt's subjects. A delivery keeps its
// original subject, under the member's own bucket, so it can never change
// another attempt's keys; the test fails if that ever changes.
func TestDeliveriesCannotChangeAnotherAttemptsKeys(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec, other := h.ensure(1), h.ensure(1)
	own, theirs := session.SnapshotBucket(string(spec.UID)), session.SnapshotBucket(string(other.UID))
	b := h.srv.lookup(other.UID).snapshot
	meta, err := b.Get(ctx, session.KeyMeta)
	require.NoError(t, err)
	m := h.connect(spec, 0)
	require.NoError(t, m.insert(ctx, session.NamespaceSnapshot, "x", []byte("Forged")))

	for _, key := range []string{session.KeyEnded, session.KeyMeta} {
		// The coordinator's connection shares the members' account, so it
		// sees what the consumer delivers.
		deliver := "$KV." + theirs + "." + key
		delivered, err := h.srv.conn.SubscribeSync(deliver)
		require.NoError(t, err)
		require.NoError(t, h.srv.conn.Flush())
		reply, err := m.nc.Request("$JS.API.CONSUMER.CREATE.KV_"+own+".forge-"+key,
			[]byte(`{"stream_name":"KV_`+own+`","config":{"name":"forge-`+key+`","deliver_subject":"`+deliver+
				`","ack_policy":"none","filter_subject":"$KV.`+own+`.snapshot.x"}}`), 2*time.Second)
		require.NoError(t, err)
		var created struct {
			Error *struct{ Description string } `json:"error"`
		}
		require.NoError(t, json.Unmarshal(reply.Data, &created))
		require.Nil(t, created.Error, "a member may create a consumer of its own stream")
		_, err = delivered.NextMsg(10 * time.Second)
		require.NoError(t, err, "the consumer must have delivered to %s", key)
		require.NoError(t, delivered.Unsubscribe())
	}

	// Had a delivery changed the other attempt's keys, its bucket would have
	// stored it before this write.
	_, err = b.Put(ctx, "snapshot.probe", nil)
	require.NoError(t, err)
	_, ok := h.ended(other)
	assert.False(t, ok, "another attempt's ended key must stay absent")
	after, err := b.Get(ctx, session.KeyMeta)
	require.NoError(t, err)
	assert.Equal(t, meta.Revision(), after.Revision(), "another attempt's meta must stay unchanged")
	assert.Equal(t, meta.Value(), after.Value(), "another attempt's meta must stay unchanged")
}

// requireViolation waits for the server to report a permission violation
// naming want.
func requireViolation(t *testing.T, violations <-chan error, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case err := <-violations:
			if errors.Is(err, nats.ErrPermissionViolation) && strings.Contains(err.Error(), want) {
				return
			}
		case <-deadline:
			t.Fatalf("the server did not refuse %s", want)
		}
	}
}
