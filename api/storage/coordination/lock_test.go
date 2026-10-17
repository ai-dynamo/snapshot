// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordination

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockerSerializesSameKey(t *testing.T) {
	l := NewLocker()
	key := Key{StoreID: "store-a", ArtifactUID: "artifact-1"}

	var active atomic.Int32
	var sawConcurrent bool
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			unlock := l.Lock(key)
			defer unlock()
			if active.Add(1) > 1 {
				mu.Lock()
				sawConcurrent = true
				mu.Unlock()
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
		})
	}
	wg.Wait()
	if sawConcurrent {
		t.Fatal("Locker allowed concurrent holders of the same key")
	}
}

func TestLockerDoesNotSerializeDifferentKeys(t *testing.T) {
	l := NewLocker()
	done := make(chan struct{})
	unlockA := l.Lock(Key{StoreID: "store-a", ArtifactUID: "artifact-1"})
	go func() {
		unlockB := l.Lock(Key{StoreID: "store-a", ArtifactUID: "artifact-2"})
		unlockB()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Locker serialized unrelated keys")
	}
	unlockA()
}

func TestLockerReleasesMapEntryWhenUncontended(t *testing.T) {
	l := NewLocker()
	key := Key{StoreID: "store-a", ArtifactUID: "artifact-1"}
	l.Lock(key)()
	l.mu.Lock()
	n := len(l.locks)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("Locker retained %d entries after release, want 0", n)
	}
}
