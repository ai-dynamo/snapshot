// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteRemovesOnlyWhatItFound checks that a Delete removes the value it
// found and no other. Another member writes while the Delete is in flight, and
// a value it inserts after the Delete's read stays.
func TestDeleteRemovesOnlyWhatItFound(t *testing.T) {
	insertNew := func(ctx context.Context, other *store) error { return other.Insert(ctx, "k", []byte("new")) }
	tests := []struct {
		name string
		// lost is set when the other member writes once the Delete's first
		// deletion has applied, and that deletion's reply is lost. It is clear
		// when the other member writes before that deletion is sent.
		lost   bool
		others func(ctx context.Context, other *store) error
	}{
		{name: "a Delete sent again after its reply was lost, past an Insert", lost: true, others: insertNew},
		{name: "a Delete overtaken by another member's Delete and Insert",
			others: func(ctx context.Context, other *store) error {
				if err := other.Delete(ctx, "k"); err != nil {
					return err
				}
				return insertNew(ctx, other)
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			b := testBucket(t)
			other := testStore(t, b)
			require.NoError(t, other.Insert(ctx, "k", []byte("old")))
			var wrote error
			kv := &overtakingKV{KeyValue: b.kv, lost: tt.lost, others: func() { wrote = tt.others(ctx, other) }}
			require.NoError(t, testStore(t, bucket{kv: kv, stream: b.stream}).Delete(ctx, "k"))
			require.NoError(t, wrote, "the other member's writes")
			got, err := other.Get(ctx, "k", 0)
			require.NoError(t, err, "a value inserted after the Delete's read must stay")
			assert.Equal(t, "new", string(got))
		})
	}
}

// overtakingKV binds a bucket's keys so that another member's writes, others,
// overtake a Delete. They land before its first deletion is sent or, if lost
// is set, once that deletion has applied. Its reply is then lost, so the
// client sends it again. A Delete runs its tries one after another, so nothing
// here needs a lock.
type overtakingKV struct {
	jetstream.KeyValue
	lost   bool
	others func()
	sent   bool
}

// Delete implements jetstream.KeyValue.
func (kv *overtakingKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if kv.sent {
		return kv.KeyValue.Delete(ctx, key, opts...)
	}
	kv.sent = true
	if !kv.lost {
		kv.others()
		return kv.KeyValue.Delete(ctx, key, opts...)
	}
	if err := kv.KeyValue.Delete(ctx, key, opts...); err != nil {
		return err
	}
	kv.others()
	// The try times out, waiting for the reply.
	return context.DeadlineExceeded
}

// testStore returns the Snapshot Store of a client of its own, over b.
func testStore(t *testing.T, b bucket) *store {
	return &store{s: testClient(t), b: b, ns: session.NamespaceSnapshot, maxValue: session.MaxValue, cost: largeRead}
}

// testBucket returns a key-value bucket of a JetStream server that runs in the
// test's process until the test ends.
func testBucket(t *testing.T) bucket {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{DontListen: true, JetStream: true, StoreDir: t.TempDir(),
		NoLog: true, NoSigs: true})
	require.NoError(t, err)
	ns.Start()
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	require.True(t, ns.ReadyForConnections(10*time.Second), "the JetStream server did not start")
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	ctx := context.Background()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "test", Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	stream, err := js.Stream(ctx, "KV_test")
	require.NoError(t, err)
	return bucket{kv: kv, stream: stream}
}
