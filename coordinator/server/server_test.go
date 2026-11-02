// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The JetStream API error codes the embedded server refuses a write with, which
// the client reports as session.ErrTooLarge and session.ErrTooMany.
const (
	codeMessageTooLarge       = 10054
	codeStoreFailed           = 10077
	codeInsufficientResources = 10023
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

// TestLimits covers the limits the embedded server enforces: each bucket's
// largest value, and the library bucket's number of keys, neither of which
// reserves memory, a message over the server's largest, and the one memory cap
// every session shares.
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

	t.Run("a message over the server's largest", func(t *testing.T) {
		h := newHarness(t, func(cfg *Config) { cfg.MaxValue = 64 << 10 })
		spec := h.ensure(1)
		nc, _ := h.raw(spec, 0)
		err := nc.Publish("$KV."+session.SnapshotBucket(string(spec.UID))+".snapshot.huge",
			make([]byte, 64<<10+payloadHeadroom+1))
		require.ErrorIs(t, err, nats.ErrMaxPayload)
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

// TestKeepaliveFitsTheGracePeriod checks the embedded server's keepalive
// against the grace period: the server gives up on a dead connection well
// before a member's grace period could run out.
func TestKeepaliveFitsTheGracePeriod(t *testing.T) {
	h := newHarness(t)
	varz, err := h.srv.ns.Varz(nil)
	require.NoError(t, err)
	assert.Equal(t, serverPingInterval, varz.PingInterval)
	assert.Equal(t, serverMaxPingsOut, varz.MaxPingsOut)
	detection := varz.PingInterval * time.Duration(varz.MaxPingsOut+1)
	assert.Greater(t, h.srv.cfg.Grace, detection)
	assert.Equal(t, DefaultGrace, h.srv.cfg.Grace)
	assert.Equal(t, session.MaxValue+payloadHeadroom, varz.MaxPayload)
	assert.Zero(t, varz.HTTPPort, "no monitoring port")
}

// TestTLSRotation serves a rotated certificate to new connections without
// restarting anything.
func TestTLSRotation(t *testing.T) {
	oldCA, oldCert := testCertificates(t)
	newCA, newCert := testCertificates(t)
	var current atomic.Pointer[tls.Certificate]
	current.Store(&oldCert)
	h := newHarness(t, func(cfg *Config) {
		cfg.TLS = &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return current.Load(), nil
			},
		}
	})
	h.ca = oldCA
	spec := h.ensure(2)
	before := h.connect(spec, 0)
	current.Store(&newCert)
	h.ca = newCA
	after := h.connect(spec, 1)
	require.NoError(t, before.Err())
	require.NoError(t, after.Err())
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

// TestConfig covers the configurations New refuses, and a replica count
// the embedded server cannot honor.
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
			require.ErrorIs(t, err, ErrInvalid)
		})
	}

	t.Run("more replicas than a server without a cluster has", func(t *testing.T) {
		h := newHarness(t, func(cfg *Config) { cfg.Replicas = 3 })
		err := h.srv.Ensure(within(t), h.spec(1))
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrInvalid))
	})
}
