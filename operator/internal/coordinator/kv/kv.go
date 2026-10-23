// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package kv defines the storage behind the Snapshot coordinator's sessions:
// for each attempt, a space of write-once keys, waits on keys, counters, and
// barriers over distinct participants.
//
// The package knows nothing about Snapshot. Roles, namespaces, quotas, members,
// retries, and failures all belong to the session package above it, which
// reaches storage only through these interfaces, so a backend such as Redis can
// replace the in-memory one without changing it. Package memory holds the
// in-memory backend, and package kvtest the conformance suite every backend
// must pass.
package kv

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/types"
)

// The errors a backend reports. Callers match them with errors.Is; a backend
// may wrap them with the key or the barrier they concern.
var (
	// ErrNotFound reports that a key does not exist.
	ErrNotFound = errors.New("key not found")

	// ErrExists reports that a key already holds a different value.
	ErrExists = errors.New("key already holds another value")

	// ErrMismatch reports that a compare-and-swap found a value other than the
	// one expected.
	ErrMismatch = errors.New("value does not match")

	// ErrNotCounter reports that a key holds a value that is not a counter.
	ErrNotCounter = errors.New("value is not a counter")

	// ErrBarrierCount reports that a barrier count is not positive, or that it
	// differs from the count the barrier already has.
	ErrBarrierCount = errors.New("barrier count mismatch")
)

// Store creates and removes each attempt's space.
type Store interface {
	// Space returns the space of the attempt identified by uid, creating it
	// empty if it does not exist yet.
	Space(uid types.UID) Space

	// Drop discards the attempt's space and everything in it, including for
	// whoever still holds the Space. The caller cancels every blocking call on
	// the space, and waits for every call to return, before dropping it, so a
	// backend need not wake anything. Dropping a space that does not exist
	// succeeds.
	Drop(ctx context.Context, uid types.UID) error
}

// Space is everything one backend implements for one attempt. Its three roles
// are separate interfaces, but always come from the same backend: checking that
// a key is absent and registering a waiter must be one atomic step, or a write
// that lands between the two is lost. Keys and barrier names are separate
// spaces, so the same name may denote both.
type Space interface {
	Values
	Watcher
	Barriers
}

// Values is a space's key-value operations. None of them blocks for longer
// than the backend takes to answer.
type Values interface {
	// Insert stores value under key if the key is absent. Inserting the value
	// a key already holds succeeds; a different value reports ErrExists.
	Insert(ctx context.Context, key string, value []byte) error

	// Get returns the value under key, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)

	// Unset removes key, whether or not it exists.
	Unset(ctx context.Context, key string) error

	// Add applies delta to the counter under key and returns the new value.
	// An absent key counts as zero, and a value that is not a counter reports
	// ErrNotCounter. See EncodeCounter for the encoding.
	Add(ctx context.Context, key string, delta int64) (int64, error)

	// CompareAndSwap replaces the value under key only if it is the one
	// expected. It reports ErrNotFound when the key is absent and ErrMismatch
	// when it holds something else.
	CompareAndSwap(ctx context.Context, key string, expected, value []byte) error
}

// Watcher waits for keys to appear.
type Watcher interface {
	// Wait returns the value under key once the key exists, at once if it
	// already does, and ctx's error as soon as ctx is done otherwise. A key
	// created by Add releases it as an Insert does.
	Wait(ctx context.Context, key string) ([]byte, error)
}

// Barriers is a space's named barriers. A barrier waits for a count of
// distinct participants, which its creator fixes. Posting to or awaiting a
// barrier before it is created is valid, and a barrier releases once and stays
// released.
type Barriers interface {
	// CreateBarrier fixes the barrier's count. Creating it again with the same
	// count succeeds; a different count, or one below one, reports
	// ErrBarrierCount.
	CreateBarrier(ctx context.Context, name string, count int) error

	// PostBarrier records one participant, which counts once however often it
	// posts.
	PostBarrier(ctx context.Context, name, participant string) error

	// AwaitBarrier returns once the barrier has released, at once if it
	// already has, and ctx's error as soon as ctx is done otherwise.
	AwaitBarrier(ctx context.Context, name string) error
}
