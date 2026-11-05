// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
)

// TestPermissionsRefuse checks every subject a member's connection must not
// reach: another member's keys, meta, another attempt's buckets, the API that
// creates and deletes buckets, other inboxes, and the system account, and that
// its own buckets refuse a rollup, which would purge them.
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
		"deleting another attempt's consumer":    "$JS.API.CONSUMER.DELETE.KV_" + theirs + ".c",
		"listing its own bucket's consumers":     "$JS.API.CONSUMER.LIST.KV_" + own,
		"reading a consumer of its own bucket":   "$JS.API.CONSUMER.INFO.KV_" + own + ".c",
		"pulling from a consumer of its own":     "$JS.API.CONSUMER.MSG.NEXT.KV_" + own + ".c",
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

	t.Run("a rollup of its own bucket", func(t *testing.T) {
		js, err := jetstream.New(nc)
		require.NoError(t, err)
		rollup := nats.NewMsg("$KV." + own + ".snapshot.x")
		rollup.Header.Set(jetstream.MsgRollup, jetstream.MsgRollupAll)
		_, err = js.PublishMsg(within(t), rollup)
		requireCode(t, err, codeRollupFailed)
		_, err = h.srv.lookup(spec.UID).snapshot.Get(within(t), session.KeyMeta)
		require.NoError(t, err, "meta must stay")
	})

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

// TestFlowControlReplies checks that a member's connection may answer the
// server's flow control on each of its buckets. The server sends a watch's
// first message whatever its size, then holds the next until its request for
// flow control is answered, so a watch that receives two large values in a row
// receives the second only if the reply went through. A refused reply changes
// nothing the session client returns, since each of its watches takes at most
// one large value, so the test watches over a raw connection.
func TestFlowControlReplies(t *testing.T) {
	h := newHarness(t, func(cfg *Config) { cfg.MaxLibraryValue = 4 << 20 })
	m := h.connect(h.ensure(1), 0)
	for _, ns := range []session.Namespace{session.NamespaceSnapshot, session.NamespaceLibrary} {
		t.Run(string(ns), func(t *testing.T) {
			ctx := within(t)
			key := string(ns) + ".big"
			w, err := m.bucket(ns).Watch(ctx, key, jetstream.UpdatesOnly())
			require.NoError(t, err)
			for _, fill := range []byte("ab") {
				_, err := m.bucket(ns).Put(ctx, key, bytes.Repeat([]byte{fill}, 3<<20))
				require.NoError(t, err)
			}
			for {
				select {
				case e := <-w.Updates():
					if e != nil && e.Value()[0] == 'b' {
						return
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("the second value never reached the watch of namespace %s", ns)
				}
			}
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

// TestDeliveriesCannotReachTheJetStreamAPI guards another attempt's buckets
// against a member that delivers its own writes to the JetStream API. Members
// share one account, which imports $JS.API.>, and JetStream does not bound a
// push consumer's deliveries by its creator's publish permissions
// (nats-server#8274; its fix, nats-server#8279, was closed, since a client
// allowed to create a consumer owns its settings). Such a delivery does reach
// the API through the import, but it keeps its original subject, under the
// member's own bucket, which names no API endpoint, so the API drops it without
// acting or answering. The test tries purging, deleting, deleting a message
// of, and updating both of another attempt's streams, and deleting one of their
// consumers, with deliveries that carry a reply subject and with ones that do
// not. It fails if the API ever audits such a delivery, or if the other
// attempt's streams, meta, consumers, or member change.
func TestDeliveriesCannotReachTheJetStreamAPI(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec, other := h.ensure(1), h.ensure(1)
	own := session.SnapshotBucket(string(spec.UID))
	m, theirs := h.connect(spec, 0), h.connect(other, 0)
	// The library bucket gets a first message, for MSG.DELETE to name, and a
	// watch, for CONSUMER.DELETE to name.
	_, err := theirs.library.Put(ctx, "library.k", []byte("v"))
	require.NoError(t, err)
	watch, err := theirs.library.Watch(context.Background(), "library.>")
	require.NoError(t, err)
	t.Cleanup(func() { _ = watch.Stop() })

	// The API audits every request it handles, such as a member's own.
	audits, err := h.srv.conn.SubscribeSync(natsserver.JSAuditAdvisory)
	require.NoError(t, err)
	require.NoError(t, h.srv.conn.Flush())
	_, err = m.nc.Request("$JS.API.STREAM.INFO.KV_"+own, nil, 2*time.Second)
	require.NoError(t, err)
	require.True(t, audited(t, audits, "$JS.API.STREAM.INFO.KV_"+own, 5*time.Second),
		"the API must audit a member's own request")

	var probe int
	buckets := []struct{ label, bucket string }{
		{"snapshot", session.SnapshotBucket(string(other.UID))},
		{"library", session.LibraryBucket(string(other.UID))},
	}
	for _, b := range buckets {
		stream := "KV_" + b.bucket
		for _, ack := range []string{"none", "explicit"} {
			for _, action := range []string{"purge", "delete", "message delete", "update", "consumer delete"} {
				t.Run(fmt.Sprintf("%s %s with ack %s", b.label, action, ack), func(t *testing.T) {
					before := h.state(t, other)
					var subject string
					var payload []byte
					switch action {
					case "purge":
						subject, payload = "$JS.API.STREAM.PURGE."+stream, []byte("{}")
					case "delete":
						subject = "$JS.API.STREAM.DELETE." + stream
					case "message delete":
						subject, payload = "$JS.API.STREAM.MSG.DELETE."+stream, []byte(`{"seq":1}`)
					case "update":
						cfg := before.Streams[stream].Config
						cfg.MaxMsgs = 1
						subject = "$JS.API.STREAM.UPDATE." + stream
						payload, err = json.Marshal(cfg)
						require.NoError(t, err)
					case "consumer delete":
						subject = "$JS.API.CONSUMER.DELETE." + stream + "." +
							h.consumerOf(t, stream, session.InboxPrefix(string(other.UID), theirs.id))
					}
					probe++
					key := fmt.Sprintf("probe%d", probe)
					delivery := m.deliver(t, h, key, payload, subject, ack)
					assert.Equal(t, "$KV."+own+".snapshot."+key, delivery.Subject,
						"a delivery must keep its original subject")
					assert.False(t, audited(t, audits, subject, pending), "the API must not handle the delivery")
					assert.Equal(t, before, h.state(t, other), "another attempt's streams and meta must stay unchanged")
					select {
					case <-theirs.done:
						t.Fatalf("another attempt's member must keep watching, but its session ended: %v", theirs.Err())
					default:
					}
				})
			}
		}
	}
}

// attemptState is what TestDeliveriesCannotReachTheJetStreamAPI compares of an
// attempt: each stream's configuration, message count, and consumers, by
// stream name, and the revision of its meta.
type attemptState struct {
	Streams map[string]streamState
	Meta    uint64
}

// streamState is what attemptState holds of one stream.
type streamState struct {
	Config    jetstream.StreamConfig
	Messages  uint64
	Consumers []string
}

// state returns the state of an attempt, which must have both of its streams.
func (h *harness) state(t *testing.T, spec Spec) attemptState {
	t.Helper()
	ctx := within(t)
	state := attemptState{Streams: make(map[string]streamState)}
	for _, bucket := range []string{session.SnapshotBucket(string(spec.UID)), session.LibraryBucket(string(spec.UID))} {
		stream, err := h.srv.js.Stream(ctx, "KV_"+bucket)
		require.NoError(t, err, "the attempt's stream must exist")
		info, err := stream.Info(ctx)
		require.NoError(t, err)
		var consumers []string
		list := stream.ListConsumers(ctx)
		for c := range list.Info() {
			consumers = append(consumers, c.Name)
		}
		require.NoError(t, list.Err())
		slices.Sort(consumers)
		state.Streams["KV_"+bucket] = streamState{Config: info.Config, Messages: info.State.Msgs, Consumers: consumers}
	}
	meta, err := h.srv.lookup(spec.UID).snapshot.Get(ctx, session.KeyMeta)
	require.NoError(t, err)
	state.Meta = meta.Revision()
	return state
}

// consumerOf returns the name of the consumer of stream that delivers to an
// inbox under prefix.
func (h *harness) consumerOf(t *testing.T, stream, prefix string) string {
	t.Helper()
	ctx := within(t)
	s, err := h.srv.js.Stream(ctx, stream)
	require.NoError(t, err)
	list := s.ListConsumers(ctx)
	var name string
	for c := range list.Info() {
		if strings.HasPrefix(c.Config.DeliverSubject, prefix+".") {
			name = c.Name
		}
	}
	require.NoError(t, list.Err())
	require.NotEmpty(t, name, "%s must have a consumer delivering under %s", stream, prefix)
	return name
}

// deliver makes the member deliver a write to subject: it creates a push
// consumer of its own Snapshot stream, filtered on key, with subject as its
// delivery subject and ack as its ack policy, then writes payload under key.
// It returns the delivery, which the coordinator's connection receives too,
// since it shares the members' account, and deletes the consumer.
func (m *member) deliver(t *testing.T, h *harness, key string, payload []byte, subject, ack string) *nats.Msg {
	t.Helper()
	ctx := within(t)
	own := "KV_" + m.snapshot.Bucket()
	delivered, err := h.srv.conn.SubscribeSync(subject)
	require.NoError(t, err)
	defer func() { _ = delivered.Unsubscribe() }()
	require.NoError(t, h.srv.conn.Flush())
	name := "deliver-" + key
	reply, err := m.nc.Request("$JS.API.CONSUMER.CREATE."+own+"."+name,
		[]byte(`{"stream_name":"`+own+`","config":{"name":"`+name+`","deliver_subject":"`+subject+
			`","ack_policy":"`+ack+`","filter_subject":"$KV.`+m.snapshot.Bucket()+`.snapshot.`+key+`"}}`),
		2*time.Second)
	require.NoError(t, err)
	var created struct {
		Error *struct{ Description string } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(reply.Data, &created))
	require.Nil(t, created.Error, "a member may create a consumer of its own stream")
	t.Cleanup(func() {
		_, _ = m.nc.Request("$JS.API.CONSUMER.DELETE."+own+"."+name, nil, 2*time.Second)
	})
	require.NoError(t, m.insert(ctx, session.NamespaceSnapshot, key, payload))
	delivery, err := delivered.NextMsg(10 * time.Second)
	require.NoError(t, err, "the consumer must have delivered to %s", subject)
	return delivery
}

// audited reports whether the API audits a request on subject within wait.
func audited(t *testing.T, audits *nats.Subscription, subject string, wait time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		msg, err := audits.NextMsg(time.Until(deadline))
		if errors.Is(err, nats.ErrTimeout) {
			return false
		}
		require.NoError(t, err)
		var audit natsserver.JSAPIAudit
		require.NoError(t, json.Unmarshal(msg.Data, &audit))
		if audit.Subject == subject {
			return true
		}
	}
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
