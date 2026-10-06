// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import "sync"

// Key identifies one (store, artifact) pair that delete, sweep and
// metadata-recovery work must serialize on. It is not a transaction ID: the
// same Key is reused across retries and across independent operation kinds.
type Key struct {
	StoreID     string
	ArtifactUID string
}

// Locker hands out per-Key exclusion so unrelated artifacts never block each
// other, while delete, sweep and recovery on the same artifact never run
// concurrently. It holds no storage state; a backend's own I/O still decides
// what a held lock permits.
type Locker struct {
	mu    sync.Mutex
	locks map[Key]*refcountedMutex
}

type refcountedMutex struct {
	mu   sync.Mutex
	refs int
}

// NewLocker returns a ready-to-use Locker.
func NewLocker() *Locker {
	return &Locker{locks: make(map[Key]*refcountedMutex)}
}

// Lock blocks until key is free, then returns an Unlock func the caller must
// call exactly once. Locked keys are released from the map once no caller
// still holds or waits on them, so Locker does not grow unbounded.
func (l *Locker) Lock(key Key) (unlock func()) {
	l.mu.Lock()
	entry, ok := l.locks[key]
	if !ok {
		entry = &refcountedMutex{}
		l.locks[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()

	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			l.mu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(l.locks, key)
			}
			l.mu.Unlock()
		})
	}
}
