// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package memory is the in-memory kv backend. Each space keeps its keys, its
// waiters, and its barriers under one mutex, and wakes waiters by closing
// channels, so a waiter blocks only in its caller's goroutine and the package
// starts none of its own.
package memory

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"k8s.io/apimachinery/pkg/types"
)

// Store is an in-memory kv.Store. It is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	spaces map[types.UID]*space
}

// New returns an empty Store.
func New() *Store {
	return &Store{spaces: make(map[types.UID]*space)}
}

// Space implements kv.Store.
func (st *Store) Space(uid types.UID) kv.Space {
	st.mu.Lock()
	defer st.mu.Unlock()
	sp, ok := st.spaces[uid]
	if !ok {
		sp = newSpace()
		st.spaces[uid] = sp
	}
	return sp
}

// Drop implements kv.Store. It empties the space as well as forgetting it, so
// whoever still holds the space, such as a session that has ended but is not
// removed yet, keeps nothing of what it held.
func (st *Store) Drop(_ context.Context, uid types.UID) error {
	st.mu.Lock()
	sp, ok := st.spaces[uid]
	delete(st.spaces, uid)
	st.mu.Unlock()
	if ok {
		sp.empty()
	}
	return nil
}

// space is one attempt's keys, waiters, and barriers, all guarded by mu.
type space struct {
	mu       sync.Mutex
	values   map[string][]byte
	waiters  map[string]*watch
	barriers map[string]*barrier
}

// watch is the broadcast channel for one key, closed when the key appears. refs
// counts the waiters holding it, so a key everyone gave up on leaves nothing
// behind.
type watch struct {
	ready chan struct{}
	refs  int
}

func newSpace() *space {
	sp := &space{}
	sp.empty()
	return sp
}

// empty replaces everything the space holds with nothing, releasing the old
// maps along with what they reference.
func (sp *space) empty() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	sp.values = make(map[string][]byte)
	sp.waiters = make(map[string]*watch)
	sp.barriers = make(map[string]*barrier)
}

// Insert implements kv.Values.
func (sp *space) Insert(_ context.Context, key string, value []byte) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if held, ok := sp.values[key]; ok {
		if !bytes.Equal(held, value) {
			return fmt.Errorf("key %q: %w", key, kv.ErrExists)
		}
		return nil
	}
	sp.set(key, bytes.Clone(value))
	return nil
}

// Get implements kv.Values. The value returned is a copy the caller may keep.
func (sp *space) Get(_ context.Context, key string) ([]byte, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	value, ok := sp.values[key]
	if !ok {
		return nil, fmt.Errorf("key %q: %w", key, kv.ErrNotFound)
	}
	return bytes.Clone(value), nil
}

// Unset implements kv.Values.
func (sp *space) Unset(_ context.Context, key string) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	delete(sp.values, key)
	return nil
}

// Add implements kv.Values.
func (sp *space) Add(_ context.Context, key string, delta int64) (int64, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var counter int64
	if held, ok := sp.values[key]; ok {
		n, err := kv.DecodeCounter(held)
		if err != nil {
			return 0, fmt.Errorf("key %q: %w", key, err)
		}
		counter = n
	}
	counter += delta
	sp.set(key, kv.EncodeCounter(counter))
	return counter, nil
}

// CompareAndSwap implements kv.Values.
func (sp *space) CompareAndSwap(_ context.Context, key string, expected, value []byte) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	held, ok := sp.values[key]
	if !ok {
		return fmt.Errorf("key %q: %w", key, kv.ErrNotFound)
	}
	if !bytes.Equal(held, expected) {
		return fmt.Errorf("key %q: %w", key, kv.ErrMismatch)
	}
	sp.values[key] = bytes.Clone(value)
	return nil
}

// Wait implements kv.Watcher. Every pass reads and registers under one lock,
// blocks until something could have changed, and starts over. Reading and
// registering together is what makes the wait safe: a value inserted between
// the two would otherwise close a channel this caller had not reached yet, and
// leave it waiting for a key that already exists. Starting over is what keeps a
// key inserted and then unset from reporting a gap.
func (sp *space) Wait(ctx context.Context, key string) ([]byte, error) {
	for {
		value, w := sp.readOrWatch(key)
		if w == nil {
			return value, nil
		}
		select {
		case <-w.ready:
			sp.unwatch(key, w)
		case <-ctx.Done():
			sp.unwatch(key, w)
			return nil, ctx.Err()
		}
	}
}

// set stores value under key and releases everything waiting on the key. The
// caller holds mu.
func (sp *space) set(key string, value []byte) {
	sp.values[key] = value
	if w, ok := sp.waiters[key]; ok {
		delete(sp.waiters, key)
		close(w.ready)
	}
}

// readOrWatch returns a copy of the value under key or, when the key is absent,
// registers the caller on the key's broadcast channel.
func (sp *space) readOrWatch(key string) ([]byte, *watch) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if value, ok := sp.values[key]; ok {
		return bytes.Clone(value), nil
	}
	w, ok := sp.waiters[key]
	if !ok {
		w = &watch{ready: make(chan struct{})}
		sp.waiters[key] = w
	}
	w.refs++
	return nil, w
}

// unwatch gives up one registration, and forgets a key nobody waits on any
// more. A watch that set already closed is no longer registered, so giving it
// up touches nothing.
func (sp *space) unwatch(key string, w *watch) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	w.refs--
	if w.refs == 0 && sp.waiters[key] == w {
		delete(sp.waiters, key)
	}
}
