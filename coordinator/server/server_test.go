// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The JetStream API error codes the embedded server refuses a write or a
// consumer with. The client reports the first three as session.ErrTooLarge and
// session.ErrTooMany, waits out the fourth, and never rolls a bucket up.
const (
	codeMessageTooLarge       = 10054
	codeStoreFailed           = 10077
	codeInsufficientResources = 10023
	codeMaximumConsumers      = 10026
	codeRollupFailed          = 10111
)

// requireCode checks that err is a JetStream API error with code.
func requireCode(t *testing.T, err error, code int, msgAndArgs ...any) {
	t.Helper()
	var apiErr *jetstream.APIError
	require.ErrorAs(t, err, &apiErr, msgAndArgs...)
	assert.Equal(t, code, int(apiErr.ErrorCode), msgAndArgs...)
}

// fill inserts values into a member's namespace until the server refuses one,
// and returns the refusal.
func fill(ctx context.Context, m *member, ns session.Namespace, size int) error {
	var err error
	for i := 0; err == nil; i++ {
		err = m.insert(ctx, ns, fmt.Sprintf("k%d", i), make([]byte, size))
	}
	return err
}

// TestLimits covers the limits the embedded server enforces on any client:
// each bucket's largest value and number of consumers, and the library
// bucket's number of keys, none of which reserves memory, and the one memory
// cap every session shares.
func TestLimits(t *testing.T) {
	t.Run("more than ten sessions fit within the default memory cap", func(t *testing.T) {
		h := newHarness(t)
		require.Equal(t, int64(DefaultMaxBytes), h.srv.cfg.MaxBytes)
		var spec Spec
		for range 20 {
			spec = h.ensure(1)
		}
		m := h.connect(spec, 0)
		require.NoError(t, m.insert(within(t), session.NamespaceSnapshot, "k", []byte("v")))
	})

	t.Run("a full library bucket refuses new keys of its session only", func(t *testing.T) {
		h := newHarness(t, func(cfg *Config) { cfg.MaxLibraryKeys = 8 })
		ctx := within(t)
		full, other := h.ensure(1), h.ensure(1)
		m := h.connect(full, 0)
		for i := range 8 {
			require.NoError(t, m.insert(ctx, session.NamespaceLibrary, fmt.Sprintf("comm-%d", i), []byte("id")))
		}
		requireCode(t, m.insert(ctx, session.NamespaceLibrary, "comm-8", []byte("id")), codeStoreFailed)
		require.NoError(t, m.library.Delete(ctx, "library.comm-0"), "an existing key takes a delete")
		require.NoError(t, m.insert(ctx, session.NamespaceLibrary, "comm-0", []byte("new id")),
			"and a new value after it")
		require.NoError(t, m.insert(ctx, session.NamespaceSnapshot, "k", []byte("v")),
			"the session's Snapshot bucket is not affected")
		require.NoError(t, h.connect(other, 0).insert(ctx, session.NamespaceLibrary, "comm-8", []byte("id")),
			"another session is not affected")
	})

	t.Run("a bucket refuses consumers past its limit, in its session only, until abandoned ones expire",
		func(t *testing.T) {
			h := newHarness(t)
			ctx := within(t)
			spec, other := h.ensure(1), h.ensure(1)
			m := h.connect(spec, 0)
			js, err := jetstream.New(m.nc)
			require.NoError(t, err)
			namespaces := []session.Namespace{session.NamespaceSnapshot, session.NamespaceLibrary}
			for _, ns := range namespaces {
				bucket := m.bucket(ns).Bucket()
				stream, err := js.Stream(ctx, "KV_"+bucket)
				require.NoError(t, err)
				info, err := stream.Info(ctx)
				require.NoError(t, err)
				// Durable consumers outlive their creator's connection, which
				// abandons them here.
				durable := func(i int) error {
					_, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Durable: fmt.Sprintf("durable-%d", i),
						AckPolicy: jetstream.AckExplicitPolicy})
					return err
				}
				for i := info.State.Consumers; i < info.Config.MaxConsumers; i++ {
					require.NoError(t, durable(i), "bucket %s", bucket)
				}
				requireCode(t, durable(info.Config.MaxConsumers), codeMaximumConsumers, "bucket %s", bucket)
				// A watch, which is how the client creates its consumers, is
				// refused with the same code, through nats.go's older API.
				_, err = m.bucket(ns).Watch(ctx, string(ns)+".k")
				var apiErr *nats.APIError
				require.ErrorAs(t, err, &apiErr, "bucket %s", bucket)
				assert.Equal(t, codeMaximumConsumers, int(apiErr.ErrorCode), "bucket %s", bucket)
			}
			h.connect(other, 0) // which watches another session's Snapshot bucket

			// Nothing pulls from the durable consumers, which free their slots
			// once they have been unused for consumerInactiveThreshold.
			for _, ns := range namespaces {
				require.Eventually(t, func() bool {
					_, err := m.bucket(ns).Watch(ctx, string(ns)+".k")
					return err == nil
				}, 3*consumerInactiveThreshold, 100*time.Millisecond, "namespace %s", ns)
			}
		})

	t.Run("a value over each cap", func(t *testing.T) {
		h := newHarness(t, func(cfg *Config) { cfg.MaxValue = 64 << 10 })
		ctx := within(t)
		m := h.connect(h.ensure(1), 0)
		// Meta carries each cap, which the client checks a value against.
		assert.Equal(t, 64<<10, m.meta.MaxValue)
		assert.Equal(t, session.MaxLibraryValue, m.meta.MaxLibraryValue)

		// A client that skips the check meets each bucket's own.
		for _, tt := range []struct {
			ns   session.Namespace
			size int
		}{
			{session.NamespaceSnapshot, 64<<10 + valueHeadroom + 1},
			{session.NamespaceLibrary, session.MaxLibraryValue + valueHeadroom + 1},
		} {
			_, err := m.bucket(tt.ns).Put(ctx, string(tt.ns)+".big", make([]byte, tt.size))
			requireCode(t, err, codeMessageTooLarge, "namespace %s", tt.ns)
		}
	})

	t.Run("the memory cap every session shares", func(t *testing.T) {
		h := newHarness(t, func(cfg *Config) { cfg.MaxBytes, cfg.MaxValue = 2<<20, 64<<10 })
		ctx := within(t)
		first, second := h.ensure(1), h.ensure(1)
		a, b := h.connect(first, 0), h.connect(second, 0)
		requireCode(t, fill(ctx, a, session.NamespaceSnapshot, 60<<10), codeInsufficientResources)
		requireCode(t, b.insert(ctx, session.NamespaceSnapshot, "k", make([]byte, 60<<10)),
			codeInsufficientResources, "the cap is shared")
		require.NoError(t, h.srv.Remove(ctx, first.UID))
		require.NoError(t, b.insert(ctx, session.NamespaceSnapshot, "k", make([]byte, 60<<10)),
			"removing a session frees its memory")
	})

	t.Run("a session that cannot open within the cap leaves no bucket behind", func(t *testing.T) {
		h := newHarness(t, func(cfg *Config) { cfg.MaxBytes, cfg.MaxValue = 2<<20, 64<<10 })
		ctx := within(t)
		filler := h.ensure(1)
		requireCode(t, fill(ctx, h.connect(filler, 0), session.NamespaceSnapshot, 60<<10),
			codeInsufficientResources)

		// Both buckets are created, since neither reserves memory; writing
		// meta is what fails.
		spec := h.spec(1)
		require.Error(t, h.srv.Ensure(ctx, spec))
		require.Nil(t, h.srv.lookup(spec.UID))
		for _, bucket := range []string{session.SnapshotBucket(string(spec.UID)),
			session.LibraryBucket(string(spec.UID))} {
			_, err := h.srv.js.KeyValue(ctx, bucket)
			require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "bucket %s", bucket)
		}
		require.NoError(t, h.srv.Remove(ctx, filler.UID))
		require.NoError(t, h.srv.Ensure(ctx, spec), "a retried Ensure starts afresh")
	})
}

// TestNoMonitoringPort checks that the embedded server opens no HTTP
// monitoring port, which would expose every connection and its subjects.
func TestNoMonitoringPort(t *testing.T) {
	h := newHarness(t)
	varz, err := h.srv.ns.Varz(nil)
	require.NoError(t, err)
	assert.Zero(t, varz.HTTPPort)
	assert.Zero(t, varz.HTTPSPort)
}

// TestLogsHoldNoSecrets drives a session, and a refused publish the server
// logs, then checks that no log line holds a key, a value, a barrier name, or
// a token.
func TestLogsHoldNoSecrets(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	spec := h.ensure(2)
	members := h.connectAll(spec)
	require.NoError(t, members[0].insert(ctx, session.NamespaceSnapshot, "secret-key", []byte("secret-value")))
	for _, m := range members {
		require.NoError(t, m.arrive(ctx, "secret-barrier"))
	}
	watchUntil(t, members[1].snapshot, session.BarrierKey("secret-barrier", "*"),
		func(present map[string]bool) bool { return len(present) == len(members) })
	nc, violations := h.raw(spec, 0)
	_, _ = nc.Request("$KV."+session.SnapshotBucket(string(spec.UID))+".barrier.secret-barrier.member-1", nil,
		200*time.Millisecond)
	requireViolation(t, violations, "secret-barrier")
	h.kick(spec, 1)
	require.NoError(t, members[0].fail(ctx, "Refused"))
	require.ErrorIs(t, awaitEnd(t, members[0]), &session.AbortError{Reason: "Refused"})

	lines := h.logs.all()
	require.True(t, slicesContain(lines, "Publish Violation"), "the refused publish must be logged")
	for _, line := range lines {
		for _, secret := range []string{"secret", agentToken, h.srv.token} {
			assert.NotContains(t, line, secret)
		}
	}
}

// slicesContain reports whether any line contains s.
func slicesContain(lines []string, s string) bool {
	for _, line := range lines {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

// TestConfig covers the configurations New refuses.
func TestConfig(t *testing.T) {
	_, cert := testCertificates(t)
	valid := Config{
		Host: "127.0.0.1", Port: -1, Authorizer: &testAuthorizer{},
		TLS: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}
	for _, tt := range []struct {
		name   string
		modify func(*Config)
	}{
		{"no TLS", func(c *Config) { c.TLS = nil }},
		{"no Authorizer", func(c *Config) { c.Authorizer = nil }},
		{"a negative limit", func(c *Config) { c.MaxLibraryKeys = -1 }},
		{"a library value over the Snapshot's", func(c *Config) { c.MaxValue, c.MaxLibraryValue = 1<<10, 2<<10 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.modify(&cfg)
			_, err := New(cfg)
			require.ErrorIs(t, err, session.ErrInvalid)
		})
	}

}
