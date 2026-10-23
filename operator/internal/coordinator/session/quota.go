// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"sync"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
)

// quotaSpace is the space as library callers see it: every write is held to the
// session's library quotas, which the ledger's usage records. Reads and waits
// pass through untouched.
//
// Each write holds the usage's mutex across its call into the space. Library
// writes are small and rare, so serializing them costs little, and it is what
// keeps the usage exact: only library callers reach library namespaces, so
// while a write holds the mutex nothing else changes what the usage mirrors.
type quotaSpace struct {
	kv.Space
	usage *usage
}

// usage is what library callers hold in one session, mirrored from the space
// so that enforcing a quota costs no extra round trip to the backend.
type usage struct {
	mu           sync.Mutex
	keys         map[string]int                 // the length of each key's value
	bytes        int                            // the sum of those lengths
	barriers     map[string]map[string]struct{} // the participants of each barrier
	participants int                            // the sum of those counts
}

// Insert implements kv.Values.
func (q quotaSpace) Insert(ctx context.Context, key string, value []byte) error {
	q.usage.mu.Lock()
	defer q.usage.mu.Unlock()
	if _, held := q.usage.keys[key]; held {
		return q.Space.Insert(ctx, key, value)
	}
	if err := q.usage.roomForKey(len(value)); err != nil {
		return fmt.Errorf("key %q: %w", key, err)
	}
	if err := q.Space.Insert(ctx, key, value); err != nil {
		return err
	}
	q.usage.setKey(key, len(value))
	return nil
}

// Unset implements kv.Values.
func (q quotaSpace) Unset(ctx context.Context, key string) error {
	q.usage.mu.Lock()
	defer q.usage.mu.Unlock()
	if err := q.Space.Unset(ctx, key); err != nil {
		return err
	}
	q.usage.unsetKey(key)
	return nil
}

// Add implements kv.Values.
func (q quotaSpace) Add(ctx context.Context, key string, delta int64) (int64, error) {
	q.usage.mu.Lock()
	defer q.usage.mu.Unlock()
	if _, held := q.usage.keys[key]; !held {
		if err := q.usage.roomForKey(kv.CounterSize); err != nil {
			return 0, fmt.Errorf("key %q: %w", key, err)
		}
	}
	counter, err := q.Space.Add(ctx, key, delta)
	if err != nil {
		return 0, err
	}
	q.usage.setKey(key, kv.CounterSize)
	return counter, nil
}

// CompareAndSwap implements kv.Values.
func (q quotaSpace) CompareAndSwap(ctx context.Context, key string, expected, value []byte) error {
	q.usage.mu.Lock()
	defer q.usage.mu.Unlock()
	if size, held := q.usage.keys[key]; held {
		if err := q.usage.roomForBytes(len(value) - size); err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	}
	if err := q.Space.CompareAndSwap(ctx, key, expected, value); err != nil {
		return err
	}
	q.usage.setKey(key, len(value))
	return nil
}

// CreateBarrier implements kv.Barriers.
func (q quotaSpace) CreateBarrier(ctx context.Context, name string, count int) error {
	q.usage.mu.Lock()
	defer q.usage.mu.Unlock()
	if _, err := q.usage.barrier(name); err != nil {
		return err
	}
	return q.Space.CreateBarrier(ctx, name, count)
}

// PostBarrier implements kv.Barriers.
func (q quotaSpace) PostBarrier(ctx context.Context, name, participant string) error {
	q.usage.mu.Lock()
	defer q.usage.mu.Unlock()
	posted, err := q.usage.barrier(name)
	if err != nil {
		return err
	}
	_, counted := posted[participant]
	if !counted {
		if err := q.usage.roomForParticipant(name, posted); err != nil {
			return err
		}
	}
	if err := q.Space.PostBarrier(ctx, name, participant); err != nil {
		return err
	}
	if !counted {
		posted[participant] = struct{}{}
		q.usage.participants++
	}
	return nil
}

// AwaitBarrier implements kv.Barriers. Awaiting may bring a barrier into being,
// so it counts against the quota, but the wait itself holds no lock.
func (q quotaSpace) AwaitBarrier(ctx context.Context, name string) error {
	q.usage.mu.Lock()
	_, err := q.usage.barrier(name)
	q.usage.mu.Unlock()
	if err != nil {
		return err
	}
	return q.Space.AwaitBarrier(ctx, name)
}

// roomForKey reports whether one more key holding size bytes fits.
func (u *usage) roomForKey(size int) error {
	if len(u.keys) >= MaxLibraryKeys {
		return fmt.Errorf("%d library keys held: %w", len(u.keys), ErrTooMany)
	}
	return u.roomForBytes(size)
}

// roomForBytes reports whether size more bytes fit. A value that shrinks always
// fits.
func (u *usage) roomForBytes(size int) error {
	if u.bytes+size > MaxLibraryBytes {
		return fmt.Errorf("%d library bytes held, %d more: %w", u.bytes, size, ErrTooMany)
	}
	return nil
}

// roomForParticipant reports whether a barrier, and the session, have room for
// one more participant.
func (u *usage) roomForParticipant(name string, posted map[string]struct{}) error {
	if len(posted) >= MaxLibraryBarrierParticipants {
		return fmt.Errorf("barrier %q has %d participants: %w", name, len(posted), ErrTooMany)
	}
	if u.participants >= MaxLibraryParticipants {
		return fmt.Errorf("%d library participants counted: %w", u.participants, ErrTooMany)
	}
	return nil
}

// barrier returns the participants counted for a barrier, counting the barrier
// itself the first time it is named. A barrier counts from then on even if the
// call that named it fails, which errs on the side of the coordinator.
func (u *usage) barrier(name string) (map[string]struct{}, error) {
	if posted, ok := u.barriers[name]; ok {
		return posted, nil
	}
	if len(u.barriers) >= MaxLibraryBarriers {
		return nil, fmt.Errorf("barrier %q after %d others: %w", name, len(u.barriers), ErrTooMany)
	}
	posted := make(map[string]struct{})
	u.barriers[name] = posted
	return posted, nil
}

// setKey records that key holds size bytes.
func (u *usage) setKey(key string, size int) {
	u.bytes += size - u.keys[key]
	u.keys[key] = size
}

// unsetKey records that key is gone.
func (u *usage) unsetKey(key string) {
	u.bytes -= u.keys[key]
	delete(u.keys, key)
}

// reset forgets everything recorded.
func (u *usage) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.keys = make(map[string]int)
	u.bytes = 0
	u.barriers = make(map[string]map[string]struct{})
	u.participants = 0
}
