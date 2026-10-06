// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import "sync"

// Key identifies one (store, artifact) pair that delete, sweep and
// metadata-recovery work must serialize on.
type Key struct {
	StoreID     string
	ArtifactUID string
}

// Locker gives per-Key exclusion so unrelated artifacts never block each
// other, while same-artifact operations never run concurrently.
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

// Lock blocks until key is free, then returns an Unlock func to call exactly
// once. Entries are released once no caller still holds or waits on them.
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
