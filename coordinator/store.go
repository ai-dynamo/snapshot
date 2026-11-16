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

	"github.com/ai-dynamo/snapshot/coordinator/session"
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
	// as it is inserted, or reports session.ErrNotYet when the wait expires. The
	// wait starts once the coordinator has answered that key is absent, so
	// however short it is, Get returns the value of a key that exists.
	// It returns early, with how the session ended, if the session ends first,
	// and with ctx's error if ctx is done first. A wait whose bucket holds its
	// most consumers waits for one to free, within the wait.
	Get(ctx context.Context, key string, wait time.Duration) ([]byte, error)

	// Delete removes key whether or not it exists. In the library bucket,
	// which limits its number of keys, deleted ones included, it writes
	// nothing if key is absent, so deleting a key that was never inserted
	// succeeds even in a full bucket. A later Insert of key starts afresh.
	// Otherwise it writes a delete marker, which takes memory, so at the
	// coordinator's memory cap it reports session.ErrTooMany, like any write:
	// a session cannot free memory by deleting its keys once the cap is
	// reached, and frees it only when the host removes it.
	Delete(ctx context.Context, key string) error
}

// Snapshot implements Session.
func (s *client) Snapshot() Store { return s.stores[session.NamespaceSnapshot] }

// CuInterpose implements Session.
func (s *client) CuInterpose() Store { return s.stores[session.NamespaceCuInterpose] }

// Library implements Session.
func (s *client) Library() Store { return s.stores[session.NamespaceLibrary] }

// newStore returns the Store of a namespace, whose keys bucket holds, bound as
// kv. Meta carries the library bucket's largest value apart from the Snapshot
// bucket's, and the library bucket limits its number of keys.
func (s *client) newStore(ns session.Namespace, bucket string, kv jetstream.KeyValue) *store {
	st := &store{s: s, kv: kv, ns: ns, maxValue: s.meta.MaxValue}
	if bucket == session.LibraryBucket(s.attempt) {
		st.maxValue, st.limitsKeys = s.meta.MaxLibraryValue, true
	}
	st.readSize = readSize(st.maxValue)
	return st
}

// readSize is what a read of a bucket whose largest value is maxValue takes of
// the connection's budget: the most its reply holds.
func readSize(maxValue int) int64 { return int64(maxValue) + session.ReplyHeadroom }

// store is one namespace of a session.
type store struct {
	s        *client
	kv       jetstream.KeyValue
	ns       session.Namespace
	maxValue int
	// readSize is what each read takes of the connection's budget.
	readSize int64
	// limitsKeys is set for a bucket that limits its number of keys, deleted
	// ones included, which a delete marker for an absent key would take one of.
	limitsKeys bool
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
	for {
		err := st.s.do(ctx, func(ctx context.Context) error {
			_, err := st.kv.Create(ctx, k, value)
			if errors.Is(err, jetstream.ErrKeyExists) {
				return session.ErrExists
			}
			return err
		})
		if !errors.Is(err, session.ErrExists) {
			return err
		}
		// Inserting the value a key holds succeeds, such as when this
		// Insert's first write landed before its connection broke.
		held, err := st.read(ctx, k)
		switch {
		case errors.Is(err, session.ErrNotFound):
			// Deleted since: create it again.
			continue
		case err != nil:
			return err
		case !bytes.Equal(held, value):
			return session.ErrExists
		}
		return nil
	}
}

// Get implements Store.
func (st *store) Get(ctx context.Context, key string, wait time.Duration) ([]byte, error) {
	k, err := st.key(key)
	if err != nil {
		return nil, err
	}
	value, err := st.read(ctx, k)
	if errors.Is(err, session.ErrNotFound) && wait > 0 {
		value, err = st.readWhenPresent(ctx, k, wait)
	}
	if err != nil {
		return nil, err
	}
	if err := st.s.settle(ctx); err != nil {
		return nil, err
	}
	return value, nil
}

// read reads the value of k, or reports session.ErrNotFound, within the
// connection's budget. Every value a store reads goes through it.
func (st *store) read(ctx context.Context, k string) ([]byte, error) {
	var value []byte
	err := st.s.read(ctx, st.readSize, func(ctx context.Context) error {
		entry, err := st.kv.Get(ctx, k)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return session.ErrNotFound
		}
		if err == nil {
			value = entry.Value()
		}
		return err
	})
	return value, err
}

// readWhenPresent waits up to wait for k to be present, then reads it, or
// reports session.ErrNotYet. The wait watches no values, so a value is only
// ever sent to answer a read, which keeps the reads of the connection within
// its limit.
func (st *store) readWhenPresent(ctx context.Context, k string, wait time.Duration) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		err := st.s.await(waitCtx, st.kv, []string{k}, func(_ string, _ []byte, present bool) bool { return present },
			jetstream.MetaOnly())
		if err == nil {
			// Read within the caller's context: the key is there, whatever
			// the wait that remains. A key deleted since is waited for again.
			var value []byte
			if value, err = st.read(ctx, k); !errors.Is(err, session.ErrNotFound) {
				return value, err
			}
			continue
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, session.ErrNotYet
		}
		return nil, err
	}
}

// Delete implements Store.
func (st *store) Delete(ctx context.Context, key string) error {
	k, err := st.key(key)
	if err != nil {
		return err
	}
	if st.limitsKeys {
		_, err := st.read(ctx, k)
		if errors.Is(err, session.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return st.s.do(ctx, func(ctx context.Context) error { return st.kv.Delete(ctx, k) })
}
