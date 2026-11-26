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

	// Delete removes the value key holds, if any. It reads key's revision and
	// deletes that revision only, so a value inserted after the read stays,
	// even when Delete is sent again after a lost reply. A later Insert of key
	// starts afresh. Deleting an absent key writes nothing, so it succeeds
	// even in a full library bucket, which limits its number of keys, deleted
	// ones included. Deleting a value writes a delete marker, which takes
	// memory, so at the coordinator's memory cap it reports
	// session.ErrTooMany, like any write: a session cannot free memory by
	// deleting its keys once the cap is reached, and frees it only when the
	// host removes it.
	Delete(ctx context.Context, key string) error
}

// Snapshot implements Session.
func (s *client) Snapshot() Store { return s.stores[session.NamespaceSnapshot] }

// CuInterpose implements Session.
func (s *client) CuInterpose() Store { return s.stores[session.NamespaceCuInterpose] }

// Library implements Session.
func (s *client) Library() Store { return s.stores[session.NamespaceLibrary] }

// newStore returns the Store of a namespace, whose keys the bucket name holds,
// bound as b. Meta carries the library bucket's largest value apart from the
// Snapshot bucket's.
func (s *client) newStore(ns session.Namespace, name string, b bucket) *store {
	st := &store{s: s, b: b, ns: ns, maxValue: s.meta.MaxValue}
	if name == session.LibraryBucket(s.attempt) {
		st.maxValue = s.meta.MaxLibraryValue
	}
	st.cost = readCost{bytes: readSize(st.maxValue), slot: name == session.SnapshotBucket(s.attempt)}
	return st
}

// readSize is what a read of a bucket whose largest value is maxValue takes of
// the connection's budget: the most its reply holds.
func readSize(maxValue int) int64 { return int64(maxValue) + session.ReplyHeadroom }

// store is one namespace of a session.
type store struct {
	s        *client
	b        bucket
	ns       session.Namespace
	maxValue int
	// cost is what each read takes of the connection's budget.
	cost readCost
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
	created, held, err := st.s.create(ctx, st.b, k, value, st.cost)
	switch {
	case err != nil:
		return err
	case created, bytes.Equal(held, value):
		// Inserting the value a key holds succeeds, such as when this
		// Insert's first write landed before its connection broke.
		return nil
	}
	return session.ErrExists
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
// connection's budget.
func (st *store) read(ctx context.Context, k string) ([]byte, error) {
	r, err := st.lookup(ctx, k)
	return r.value, err
}

// lookup reads the latest record of k, or reports session.ErrNotFound if k is
// absent, within the connection's budget.
func (st *store) lookup(ctx context.Context, k string) (record, error) {
	var r record
	if err := st.s.read(ctx, st.cost, latest(st.b, k, &r)); err != nil {
		return record{}, err
	}
	if r.deleted {
		return record{}, session.ErrNotFound
	}
	return r, nil
}

// readWhenPresent waits up to wait for k to be present, then reads it, or
// reports session.ErrNotYet. The wait watches no values, so a value is only
// ever sent to answer a read, which keeps the reads of the connection within
// its limit.
func (st *store) readWhenPresent(ctx context.Context, k string, wait time.Duration) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		err := st.s.await(waitCtx, st.b.kv, []string{k}, func(_ string, _ []byte, present bool) bool { return present },
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
	found, err := st.lookup(ctx, k)
	if errors.Is(err, session.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return st.s.write(ctx, func(ctx context.Context) error {
		err := st.b.kv.Delete(ctx, k, jetstream.LastRevision(found.revision))
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			// The value found is gone: an earlier try deleted it and its
			// reply was lost, or another member deleted it, and may have
			// inserted a newer value, which stays.
			return nil
		}
		return err
	})
}
