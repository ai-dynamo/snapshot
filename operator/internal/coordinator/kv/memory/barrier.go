// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"fmt"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
)

// barrier is a named count of distinct participants. count is zero until the
// barrier is created, which is why creating it with zero is refused: that value
// means nobody has fixed the count yet.
type barrier struct {
	count    int
	posted   map[string]struct{}
	done     chan struct{}
	released bool
}

// CreateBarrier implements kv.Barriers.
func (sp *space) CreateBarrier(_ context.Context, name string, count int) error {
	if count < 1 {
		return fmt.Errorf("barrier %q with a count of %d: %w", name, count, kv.ErrBarrierCount)
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	b := sp.barrier(name)
	if b.count != 0 && b.count != count {
		return fmt.Errorf("barrier %q was created with %d, not %d: %w", name, b.count, count, kv.ErrBarrierCount)
	}
	b.count = count
	b.release()
	return nil
}

// PostBarrier implements kv.Barriers.
func (sp *space) PostBarrier(_ context.Context, name, participant string) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	b := sp.barrier(name)
	b.posted[participant] = struct{}{}
	b.release()
	return nil
}

// AwaitBarrier implements kv.Barriers. A barrier already released wins over a
// done context, so the answer for a released barrier never depends on timing.
func (sp *space) AwaitBarrier(ctx context.Context, name string) error {
	sp.mu.Lock()
	b := sp.barrier(name)
	sp.mu.Unlock()

	select {
	case <-b.done:
		return nil
	default:
	}
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// barrier returns the named barrier, which posting or awaiting may bring into
// being before anyone creates it. The caller holds mu.
func (sp *space) barrier(name string) *barrier {
	b, ok := sp.barriers[name]
	if !ok {
		b = &barrier{posted: make(map[string]struct{}), done: make(chan struct{})}
		sp.barriers[name] = b
	}
	return b
}

// release wakes the barrier's waiters once it is created and every participant
// has posted. It runs once; the barrier stays released.
func (b *barrier) release() {
	if b.released || b.count == 0 || len(b.posted) < b.count {
		return
	}
	b.released = true
	close(b.done)
}
