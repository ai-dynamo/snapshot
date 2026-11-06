// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
)

// minKeyFetchInterval is the cooldown on fetching the issuer's keys. A token
// the keys held do not verify starts a fetch only when the last one started at
// least this long before, whether it succeeded or not; within it, the token is
// refused without one. So each coordinator makes at most one key fetch every
// 10 s, however many tokens it is sent and whatever key IDs they name.
//
// It is a cooldown, not a promise of admission. After the API server starts
// signing with a new key, an agent is admitted only once a fetch has succeeded
// and its retry passes the TokenReview. Clients retry every refusal until
// their own deadline.
const minKeyFetchInterval = 10 * time.Second

// keySet is the issuer's keys, as an oidc.KeySet. It verifies a token against
// the keys it holds, and fetches them again, at most once every
// minKeyFetchInterval, when they do not verify it.
type keySet struct {
	client *http.Client
	url    string
	clock  clock.PassiveClock
	log    logr.Logger

	// keys are the keys held, which verification reads without a lock.
	keys atomic.Pointer[oidc.StaticKeySet]

	mu sync.Mutex
	// lastFetch is when the last fetch started, and fetching, while a fetch
	// is in flight, is closed when it ends.
	lastFetch time.Time
	fetching  chan struct{}
}

// newKeySet fetches the keys at url through client, within ctx, and returns a
// keySet that holds them. The fetch starts the cooldown.
func newKeySet(ctx context.Context, client *http.Client, url string, cfg Config) (*keySet, error) {
	s := &keySet{client: client, url: url, clock: cfg.Clock, log: cfg.Log, lastFetch: cfg.Clock.Now()}
	keys, err := s.fetch(ctx)
	if err != nil {
		return nil, err
	}
	s.keys.Store(keys)
	return s, nil
}

// VerifySignature implements oidc.KeySet. A token the keys held verify never
// waits for a fetch. Any other waits, within ctx, for the fetch in flight or
// for one it may start, and is verified once more against the result.
func (s *keySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	held := s.keys.Load()
	if payload, err := held.VerifySignature(ctx, jwt); err == nil {
		return payload, nil
	}
	fetched, err := s.refresh(ctx, held)
	if err != nil {
		return nil, err
	}
	select {
	case <-fetched:
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for a fetch of the issuer's keys: %w", context.Cause(ctx))
	}
	if payload, err := s.keys.Load().VerifySignature(ctx, jwt); err == nil {
		return payload, nil
	}
	return nil, errors.New("none of the issuer's keys verifies the token")
}

// refresh returns a channel that is closed once a caller that read held has
// the newest keys it can get: at once if a fetch has replaced held since, or
// else when the fetch in flight, or one refresh starts, ends. Within the
// cooldown it starts none, and returns an error.
func (s *keySet) refresh(ctx context.Context, held *oidc.StaticKeySet) (<-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.fetching != nil:
		return s.fetching, nil
	case s.keys.Load() != held:
		// A fetch replaced the keys after the caller read them.
		done := make(chan struct{})
		close(done)
		return done, nil
	}
	if since := s.clock.Since(s.lastFetch); since < minKeyFetchInterval {
		return nil, fmt.Errorf("none of the issuer's keys verifies the token, and the last fetch of them started %v ago",
			since)
	}
	s.lastFetch = s.clock.Now()
	s.fetching = make(chan struct{})
	go s.update(context.WithoutCancel(ctx), s.fetching)
	return s.fetching, nil
}

// update fetches the keys under a context of its own, so that a caller giving
// up fails none of the others waiting, holds them if the fetch succeeds, and
// closes done.
func (s *keySet) update(ctx context.Context, done chan struct{}) {
	ctx, cancel := context.WithTimeout(ctx, server.AuthorizeTimeout)
	defer cancel()
	keys, err := s.fetch(ctx)
	if err != nil {
		s.log.Error(err, "A fetch of the issuer's keys failed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.keys.Store(keys)
	}
	s.fetching = nil
	close(done)
}

// fetch gets the key set, within ctx, and returns the keys in it that can
// verify a signature. It fails when there is none.
func (s *keySet) fetch(ctx context.Context) (*oidc.StaticKeySet, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := getJSON(ctx, s.client, s.url, &set); err != nil {
		return nil, err
	}
	keys := &oidc.StaticKeySet{}
	for _, raw := range set.Keys {
		// A key set may hold keys of other kinds and uses, which RFC 7517
		// says to ignore.
		var k jose.JSONWebKey
		if json.Unmarshal(raw, &k) == nil && k.IsPublic() && k.Valid() && (k.Use == "" || k.Use == "sig") {
			keys.PublicKeys = append(keys.PublicKeys, k.Key)
		}
	}
	if len(keys.PublicKeys) == 0 {
		return nil, fmt.Errorf("GET %s: no key in the key set can verify a signature", s.url)
	}
	return keys, nil
}
