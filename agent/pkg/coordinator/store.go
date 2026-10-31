// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ai-dynamo/snapshot/agent/pkg/session"
	"github.com/nats-io/nats.go/jetstream"
)

// Store is one namespace of a session. The agent hands the relay the
// cuInterpose Store and its library endpoint the library Store, which confines
// them.
type Store interface {
	// Insert stores value under key if key is absent. If key already holds
	// the same value it succeeds, and if it holds a different value it reports
	// session.ErrExists. A value over its bucket's largest reports
	// session.ErrTooLarge, and a new key past the library bucket's most keys,
	// or a write past the coordinator's memory cap, session.ErrTooMany.
	Insert(ctx context.Context, key string, value []byte) error

	// Get returns the value of key. If key is absent, with no wait it reports
	// session.ErrNotFound at once, and with a wait it returns the value as soon
	// as it is inserted, or reports session.ErrNotYet when the wait expires.
	// It returns early, with how the session ended, if the session ends first,
	// and with ctx's error if ctx is done first.
	Get(ctx context.Context, key string, wait time.Duration) ([]byte, error)

	// Delete removes key whether or not it exists. A later Insert of key
	// starts afresh.
	Delete(ctx context.Context, key string) error
}

// Namespace implements Session.
func (s *client) Namespace(ns session.Namespace) Store {
	switch ns {
	case session.NamespaceSnapshot, session.NamespaceCuInterpose:
		return &store{s: s, kv: s.snapshot, ns: ns, maxValue: s.meta.MaxValue}
	case session.NamespaceLibrary:
		return &store{s: s, kv: s.library, ns: ns, maxValue: s.meta.MaxLibraryValue}
	default:
		return invalidStore{ns: ns}
	}
}

// store is one namespace of a session.
type store struct {
	s        *client
	kv       jetstream.KeyValue
	ns       session.Namespace
	maxValue int
}

// key returns the bucket key of a caller's key.
func (st *store) key(key string) (string, error) {
	switch {
	case key == "":
		return "", fmt.Errorf("an empty key: %w", session.ErrInvalid)
	case len(key) > MaxKeyLength:
		return "", fmt.Errorf("a key of %d bytes is over the %d-byte limit: %w", len(key), MaxKeyLength,
			session.ErrTooLarge)
	case st.ns == session.NamespaceLibrary:
		return encodeLibraryKey(key), nil
	}
	if err := validateKey(key); err != nil {
		return "", err
	}
	return string(st.ns) + "." + key, nil
}

// keyRE matches the characters a JetStream key allows.
var keyRE = regexp.MustCompile(`^[A-Za-z0-9_=/.-]+$`)

// validateKey reports whether key may follow a namespace in the Snapshot
// bucket: one or more dot-separated tokens of the characters JetStream allows.
func validateKey(key string) error {
	if !keyRE.MatchString(key) || strings.HasPrefix(key, ".") || strings.HasSuffix(key, ".") ||
		strings.Contains(key, "..") {
		return fmt.Errorf("a key must be dot-separated tokens of letters, digits, and -/_=: %w", session.ErrInvalid)
	}
	return nil
}

// encodeLibraryKey turns a library's key, which may hold any bytes, into a key
// the library bucket accepts, encoded with unpadded base64url: JetStream keys
// allow only letters, digits, and -/_=. characters.
func encodeLibraryKey(key string) string {
	return string(session.NamespaceLibrary) + "." + base64.RawURLEncoding.EncodeToString([]byte(key))
}

// Insert implements Store.
func (st *store) Insert(ctx context.Context, key string, value []byte) error {
	k, err := st.key(key)
	if err != nil {
		return err
	}
	if len(value) > st.maxValue {
		return fmt.Errorf("a value of %d bytes is over the %d-byte limit: %w", len(value), st.maxValue,
			session.ErrTooLarge)
	}
	if err := st.s.alive(); err != nil {
		return err
	}
	return st.s.do(ctx, func(ctx context.Context) error {
		_, err := st.kv.Create(ctx, k, value)
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return err
		}
		entry, err := st.kv.Get(ctx, k)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Deleted since: create it again.
			return errRetry
		case err != nil:
			return err
		case bytes.Equal(entry.Value(), value):
			return nil
		default:
			return session.ErrExists
		}
	})
}

// Get implements Store.
func (st *store) Get(ctx context.Context, key string, wait time.Duration) ([]byte, error) {
	k, err := st.key(key)
	if err != nil {
		return nil, err
	}
	if err := st.s.alive(); err != nil {
		return nil, err
	}
	var value []byte
	if wait <= 0 {
		err = st.s.do(ctx, func(ctx context.Context) error {
			entry, err := st.kv.Get(ctx, k)
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				return session.ErrNotFound
			}
			if err == nil {
				value = entry.Value()
			}
			return err
		})
	} else {
		waitCtx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		err = st.s.await(waitCtx, st.kv, []string{k}, func(_ string, v []byte, present bool) bool {
			value = v
			return present
		})
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, session.ErrNotYet
		}
	}
	if err != nil {
		return nil, err
	}
	if err := st.s.settle(ctx); err != nil {
		return nil, err
	}
	return value, nil
}

// Delete implements Store.
func (st *store) Delete(ctx context.Context, key string) error {
	k, err := st.key(key)
	if err != nil {
		return err
	}
	if err := st.s.alive(); err != nil {
		return err
	}
	return st.s.do(ctx, func(ctx context.Context) error {
		return st.kv.Delete(ctx, k)
	})
}

// invalidStore is the Store of an unknown namespace.
type invalidStore struct{ ns session.Namespace }

func (i invalidStore) err() error {
	return fmt.Errorf("unknown namespace %q: %w", i.ns, session.ErrInvalid)
}

// Insert implements Store.
func (i invalidStore) Insert(context.Context, string, []byte) error { return i.err() }

// Get implements Store.
func (i invalidStore) Get(context.Context, string, time.Duration) ([]byte, error) {
	return nil, i.err()
}

// Delete implements Store.
func (i invalidStore) Delete(context.Context, string) error { return i.err() }
