// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForgedTokensCostOneKeyFetch(t *testing.T) {
	h := newHarness(t)
	require.EqualValues(t, 1, h.issuer.keyFetches.Load(), "New fetches the keys")
	h.clock.Step(minKeyFetchInterval)

	// Each names the issuer's key ID, no key ID, or a key ID of its own.
	const n = 100
	for i := range n {
		id := []string{"key-0", "", fmt.Sprintf("forged-%d", i)}[i%3]
		token := forge(t, id, h.issuer.claims(h.clock.Now()))
		require.ErrorIs(t, h.authorize(member, token), server.ErrUnauthenticated)
		h.clock.Step((minKeyFetchInterval - time.Nanosecond) / n)
	}
	assert.EqualValues(t, 2, h.issuer.keyFetches.Load())
	assert.Zero(t, h.placement.asked.Load(), "asked where the members run")
	assert.Zero(t, h.reviewer.calls.Load(), "reviewed a token")
}

func TestFailedKeyFetchesAreThrottled(t *testing.T) {
	t.Run("after a failed fetch", func(t *testing.T) {
		h := newHarness(t)
		h.issuer.addKey(t, "key-1")
		h.issuer.respondWith(unavailable)
		h.clock.Step(minKeyFetchInterval)
		token := h.signedWith("key-1")
		require.ErrorIs(t, h.authorize(member, token), server.ErrUnauthenticated)
		require.EqualValues(t, 2, h.issuer.keyFetches.Load())

		// The key set is back, but no fetch starts until the cooldown has
		// passed since the failed one started.
		h.issuer.respondWith(nil)
		h.clock.Step(minKeyFetchInterval - time.Nanosecond)
		require.ErrorIs(t, h.authorize(member, token), server.ErrUnauthenticated)
		require.EqualValues(t, 2, h.issuer.keyFetches.Load())
		h.clock.Step(time.Nanosecond)
		require.NoError(t, h.authorize(member, token))
		assert.EqualValues(t, 3, h.issuer.keyFetches.Load())
	})
	t.Run("from the start of a fetch", func(t *testing.T) {
		h := newHarness(t)
		h.issuer.addKey(t, "key-1")
		h.issuer.respondWith(unavailable)
		release := h.issuer.holdKeys(t)
		h.clock.Step(minKeyFetchInterval)
		token := h.signedWith("key-1")
		refused := make(chan error, 1)
		go func() { refused <- h.authorize(member, token) }()
		require.Eventually(t, func() bool { return h.issuer.keyFetches.Load() == 2 }, time.Second, time.Millisecond)

		// The fetch takes the whole cooldown, and fails.
		h.clock.Step(minKeyFetchInterval)
		release()
		require.ErrorIs(t, <-refused, server.ErrUnauthenticated)

		// The cooldown has passed since it started.
		h.issuer.respondWith(nil)
		require.NoError(t, h.authorize(member, token))
		assert.EqualValues(t, 3, h.issuer.keyFetches.Load())
	})
}

func TestTheKeysHeldVerifyWithoutWaiting(t *testing.T) {
	t.Run("after a failed fetch", func(t *testing.T) {
		h := newHarness(t)
		h.issuer.respondWith(unavailable)
		h.clock.Step(minKeyFetchInterval)
		require.ErrorIs(t, h.authorize(member, h.signedWith("key-unknown")), server.ErrUnauthenticated)
		require.EqualValues(t, 2, h.issuer.keyFetches.Load())

		require.NoError(t, h.authorize(member, h.token()))
		assert.EqualValues(t, 2, h.issuer.keyFetches.Load())
	})
	t.Run("while a fetch is in flight", func(t *testing.T) {
		h := newHarness(t)
		h.issuer.addKey(t, "key-1")
		release := h.issuer.holdKeys(t)
		h.clock.Step(minKeyFetchInterval)
		token := h.signedWith("key-1")
		waiting := make(chan error, 1)
		go func() { waiting <- h.authorize(member, token) }()
		require.Eventually(t, func() bool { return h.issuer.keyFetches.Load() == 2 }, time.Second, time.Millisecond)

		// The fetch is held until the test releases it, after this.
		require.NoError(t, h.authorize(member, h.token()))

		release()
		require.NoError(t, <-waiting)
		assert.EqualValues(t, 2, h.issuer.keyFetches.Load())
	})
}

func TestConcurrentCallersShareOneKeyFetch(t *testing.T) {
	h := newHarness(t)
	h.issuer.addKey(t, "key-1")
	release := h.issuer.holdKeys(t)
	h.clock.Step(minKeyFetchInterval)

	// The first caller starts the fetch.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	first := make(chan error, 1)
	token := h.signedWith("key-1")
	go func() { first <- h.authorizeWithin(ctx, member, token) }()
	require.Eventually(t, func() bool { return h.issuer.keyFetches.Load() == 2 }, time.Second, time.Millisecond)

	// The others wait for it.
	const n = 8
	errs := make(chan error, n)
	for range n {
		token := h.signedWith("key-1")
		go func() { errs <- h.authorize(member, token) }()
	}

	// The first gives up at its deadline, while the fetch is still held.
	err := <-first
	require.ErrorIs(t, err, server.ErrUnavailable)
	assert.ErrorContains(t, err, "waiting for a fetch of the issuer's keys: context deadline exceeded")

	// The others are admitted once it ends.
	release()
	for range n {
		require.NoError(t, <-errs)
	}
	assert.EqualValues(t, 2, h.issuer.keyFetches.Load())
	assert.EqualValues(t, n, h.reviewer.calls.Load())
}

func TestRotatedKeysAreFetchedOnFirstUse(t *testing.T) {
	h := newHarness(t)
	require.EqualValues(t, 1, h.issuer.keyFetches.Load(), "New fetches the keys")

	// New's fetch starts the cooldown too.
	h.issuer.addKey(t, "key-1")
	first := h.signedWith("key-1")
	require.ErrorIs(t, h.authorize(member, first), server.ErrUnauthenticated)
	require.EqualValues(t, 1, h.issuer.keyFetches.Load())
	h.clock.Step(minKeyFetchInterval)
	require.NoError(t, h.authorize(member, first))
	require.EqualValues(t, 2, h.issuer.keyFetches.Load())

	h.issuer.addKey(t, "key-2")
	second := h.signedWith("key-2")
	require.ErrorIs(t, h.authorize(member, second), server.ErrUnauthenticated)
	h.clock.Step(minKeyFetchInterval - time.Nanosecond)
	require.ErrorIs(t, h.authorize(member, second), server.ErrUnauthenticated)
	require.EqualValues(t, 2, h.issuer.keyFetches.Load())
	h.clock.Step(time.Nanosecond)
	require.NoError(t, h.authorize(member, second))
	require.EqualValues(t, 3, h.issuer.keyFetches.Load())

	// The keys fetched still hold the earlier ones.
	require.NoError(t, h.authorize(member, h.signedWith("key-1")))
	require.NoError(t, h.authorize(member, h.token()))
	assert.EqualValues(t, 3, h.issuer.keyFetches.Load())
}

func TestBadKeySetsKeepTheKeysHeld(t *testing.T) {
	for name, respond := range map[string]func(http.ResponseWriter, jose.JSONWebKeySet){
		"an error":  unavailable,
		"not JSON":  func(w http.ResponseWriter, _ jose.JSONWebKeySet) { _, _ = io.WriteString(w, "not JSON") },
		"no keys":   func(w http.ResponseWriter, _ jose.JSONWebKeySet) { _, _ = io.WriteString(w, `{}`) },
		"no key":    func(w http.ResponseWriter, _ jose.JSONWebKeySet) { _, _ = io.WriteString(w, `{"keys":[]}`) },
		"no usable": func(w http.ResponseWriter, set jose.JSONWebKeySet) { writeJSON(w, unusableKeys(set)) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.issuer.addKey(t, "key-1")
			h.issuer.respondWith(respond)
			h.clock.Step(minKeyFetchInterval)
			require.ErrorIs(t, h.authorize(member, h.signedWith("key-1")), server.ErrUnauthenticated)
			require.EqualValues(t, 2, h.issuer.keyFetches.Load())

			require.NoError(t, h.authorize(member, h.token()))
			h.mu.Lock()
			defer h.mu.Unlock()
			require.Len(t, h.logs, 1)
			assert.Contains(t, h.logs[0], `"msg"="A fetch of the issuer's keys failed"`)
		})
	}
}

func TestKeySetsMayHoldKeysOfOtherKinds(t *testing.T) {
	h := newHarness(t)
	h.issuer.addKey(t, "key-1")
	h.issuer.respondWith(func(w http.ResponseWriter, set jose.JSONWebKeySet) {
		keys := unusableKeys(set)
		for _, k := range set.Keys {
			keys["keys"] = append(keys["keys"], k)
		}
		writeJSON(w, keys)
	})
	h.clock.Step(minKeyFetchInterval)
	require.NoError(t, h.authorize(member, h.signedWith("key-1")))
	assert.EqualValues(t, 2, h.issuer.keyFetches.Load())
}

// unusableKeys returns a key set of keys that cannot verify a signature: a
// symmetric key, an EC key with no point, a key of an unknown type, and the
// issuer's keys, marked for encryption.
func unusableKeys(set jose.JSONWebKeySet) map[string][]any {
	keys := []any{
		map[string]string{"kty": "oct", "k": "bm90LWEta2V5"},
		map[string]string{"kty": "EC", "crv": "P-256"},
		map[string]string{"kty": "unknown"},
	}
	for _, k := range set.Keys {
		k.Use = "enc"
		keys = append(keys, k)
	}
	return map[string][]any{"keys": keys}
}

// A caller whose token the keys it read do not verify, and which asks for a
// fetch after one has replaced them, verifies it again at once, even within
// the cooldown.
func TestAFetchServesCallersThatReadTheKeysBeforeIt(t *testing.T) {
	h := newHarness(t)
	s, err := newKeySet(t.Context(), h.issuer.srv.Client(), h.issuer.url()+keysPath, h.config())
	require.NoError(t, err)
	held := s.keys.Load()

	h.issuer.addKey(t, "key-1")
	h.clock.Step(minKeyFetchInterval)
	_, err = s.VerifySignature(t.Context(), h.signedWith("key-1"))
	require.NoError(t, err)
	fetches := h.issuer.keyFetches.Load()

	fetched, err := s.refresh(t.Context(), held)
	require.NoError(t, err)
	select {
	case <-fetched:
	default:
		t.Fatal("refresh waits for a fetch that has ended")
	}
	assert.Equal(t, fetches, h.issuer.keyFetches.Load())
}
