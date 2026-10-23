// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
)

// What the budget charges for each kind of entry beyond the bytes the entry
// names. Each is an estimate, rounded up, of what the memory backend and the
// session's own bookkeeping spend on one such entry: map entries, string and
// slice headers, and the structs behind them. They make a flood of tiny entries
// cost what it really costs, not just its bytes.
const (
	// keyOverhead is one key: its map entry in the space and in the
	// budget's mirror, with their headers.
	keyOverhead = 96

	// barrierOverhead is one barrier: its struct, its participant set, and
	// its channel in the space, and its entry in the mirror.
	barrierOverhead = 256

	// participantOverhead is one participant: its set entry in the space and
	// in the mirror.
	participantOverhead = 64

	// requestOverhead is one recorded request: its map entry, keyed by the
	// caller and the id, and the outcome it keeps.
	requestOverhead = 192
)

// cost is what the budget charges for one entry: the bytes it names, plus its
// overhead.
func cost(overhead int, sizes ...int) int64 {
	total := overhead
	for _, size := range sizes {
		total += size
	}
	return int64(total)
}

// budget is the bytes a session holds, which MaxSessionBytes bounds. The
// space's keys, barriers, and participants, and the ledger's recorded requests,
// all charge it. It is a counter rather than a field behind a mutex, so the
// ledger charges it under its own mutex and no two of the session's mutexes
// are ever held together.
type budget struct {
	used atomic.Int64
}

// charge takes n bytes, or reports ErrTooMany when they do not fit.
func (b *budget) charge(n int64) error {
	for {
		used := b.used.Load()
		if used+n > MaxSessionBytes {
			return fmt.Errorf("%d bytes held, %d more: %w", used, n, ErrTooMany)
		}
		if b.used.CompareAndSwap(used, used+n) {
			return nil
		}
	}
}

// refund gives n bytes back.
func (b *budget) refund(n int64) {
	b.used.Add(-n)
}

// usage mirrors what the space holds, so a write is charged what it adds and an
// unset gives back what it frees, without an extra round trip to the backend.
type usage struct {
	mu       sync.Mutex
	keys     map[string]int64               // what each key is charged
	barriers map[string]map[string]struct{} // the participants charged per barrier
}

// reset forgets everything mirrored.
func (u *usage) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.keys = make(map[string]int64)
	u.barriers = make(map[string]map[string]struct{})
}

// budgetSpace is the space as every caller writes it: each write is charged to
// the session's budget before it reaches the backend, and an unset or a value
// that shrinks gives bytes back. Reads and waits pass through.
//
// A write holds the usage's mutex across its call to the backend. That keeps
// the mirror exact, since nothing else changes a key while a write charges it,
// and costs little with the memory backend, which answers at once; a backend
// with round trips would serialize one session's writes. The decorator names
// every method of kv.Space instead of embedding it, so a method added to the kv
// interfaces later fails to compile here rather than bypassing the budget.
type budgetSpace struct {
	space  kv.Space
	budget *budget
	usage  *usage
}

var _ kv.Space = budgetSpace{}

// Insert implements kv.Values. Inserting a key that exists changes nothing the
// budget counts, whether it succeeds or reports ErrExists.
func (b budgetSpace) Insert(ctx context.Context, key string, value []byte) error {
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	if _, held := b.usage.keys[key]; held {
		return b.space.Insert(ctx, key, value)
	}
	charge := cost(keyOverhead, len(key), len(value))
	if err := b.budget.charge(charge); err != nil {
		return fmt.Errorf("key %q: %w", key, err)
	}
	if err := b.space.Insert(ctx, key, value); err != nil {
		b.budget.refund(charge)
		return err
	}
	b.usage.keys[key] = charge
	return nil
}

// Get implements kv.Values.
func (b budgetSpace) Get(ctx context.Context, key string) ([]byte, error) {
	return b.space.Get(ctx, key)
}

// Unset implements kv.Values.
func (b budgetSpace) Unset(ctx context.Context, key string) error {
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	if err := b.space.Unset(ctx, key); err != nil {
		return err
	}
	if charge, held := b.usage.keys[key]; held {
		b.budget.refund(charge)
		delete(b.usage.keys, key)
	}
	return nil
}

// Add implements kv.Values. A counter is charged once, when Add creates it.
func (b budgetSpace) Add(ctx context.Context, key string, delta int64) (int64, error) {
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	_, held := b.usage.keys[key]
	charge := cost(keyOverhead, len(key), kv.CounterSize)
	if !held {
		if err := b.budget.charge(charge); err != nil {
			return 0, fmt.Errorf("key %q: %w", key, err)
		}
	}
	counter, err := b.space.Add(ctx, key, delta)
	if err != nil {
		if !held {
			b.budget.refund(charge)
		}
		return 0, err
	}
	if !held {
		b.usage.keys[key] = charge
	}
	return counter, nil
}

// CompareAndSwap implements kv.Values. A value that grows is charged the
// difference, and one that shrinks gives it back.
func (b budgetSpace) CompareAndSwap(ctx context.Context, key string, expected, value []byte) error {
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	held, ok := b.usage.keys[key]
	if !ok {
		// The key is absent, which the backend reports.
		return b.space.CompareAndSwap(ctx, key, expected, value)
	}
	next := cost(keyOverhead, len(key), len(value))
	growth := next - held
	if growth > 0 {
		if err := b.budget.charge(growth); err != nil {
			return fmt.Errorf("key %q: %w", key, err)
		}
	}
	if err := b.space.CompareAndSwap(ctx, key, expected, value); err != nil {
		if growth > 0 {
			b.budget.refund(growth)
		}
		return err
	}
	if growth < 0 {
		b.budget.refund(-growth)
	}
	b.usage.keys[key] = next
	return nil
}

// Wait implements kv.Watcher.
func (b budgetSpace) Wait(ctx context.Context, key string) ([]byte, error) {
	return b.space.Wait(ctx, key)
}

// CreateBarrier implements kv.Barriers.
func (b budgetSpace) CreateBarrier(ctx context.Context, name string, count int) error {
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	if _, err := b.nameLocked(name); err != nil {
		return err
	}
	return b.space.CreateBarrier(ctx, name, count)
}

// PostBarrier implements kv.Barriers. A participant is charged once per barrier.
func (b budgetSpace) PostBarrier(ctx context.Context, name, participant string) error {
	b.usage.mu.Lock()
	defer b.usage.mu.Unlock()
	posted, err := b.nameLocked(name)
	if err != nil {
		return err
	}
	_, counted := posted[participant]
	charge := cost(participantOverhead, len(participant))
	if !counted {
		if err := b.budget.charge(charge); err != nil {
			return fmt.Errorf("barrier %q: %w", name, err)
		}
	}
	if err := b.space.PostBarrier(ctx, name, participant); err != nil {
		if !counted {
			b.budget.refund(charge)
		}
		return err
	}
	posted[participant] = struct{}{}
	return nil
}

// AwaitBarrier implements kv.Barriers. Awaiting may bring a barrier into being,
// so it names the barrier, but the wait itself holds no lock.
func (b budgetSpace) AwaitBarrier(ctx context.Context, name string) error {
	b.usage.mu.Lock()
	_, err := b.nameLocked(name)
	b.usage.mu.Unlock()
	if err != nil {
		return err
	}
	return b.space.AwaitBarrier(ctx, name)
}

// nameLocked returns the participants charged for a barrier, charging the
// barrier itself the first time anything names it. The charge stays even if the
// call that named the barrier fails, which errs on the coordinator's side. The
// caller holds the usage's mutex.
func (b budgetSpace) nameLocked(name string) (map[string]struct{}, error) {
	if posted, ok := b.usage.barriers[name]; ok {
		return posted, nil
	}
	if err := b.budget.charge(cost(barrierOverhead, len(name))); err != nil {
		return nil, fmt.Errorf("barrier %q: %w", name, err)
	}
	posted := make(map[string]struct{})
	b.usage.barriers[name] = posted
	return posted, nil
}
