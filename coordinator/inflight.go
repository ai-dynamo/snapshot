// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"sync"
)

// inflight tracks the operations waiting for the coordinator's answer. A
// request sent just before the connection broke is lost with it, so a
// disconnect makes every operation send its request again, which is safe for
// every operation, instead of waiting out its timeout.
type inflight struct {
	mu   sync.Mutex
	next uint64
	ops  map[uint64]context.CancelCauseFunc
}

// add tracks an operation, which interrupt ends through cancel. The returned
// function stops tracking it.
func (f *inflight) add(cancel context.CancelCauseFunc) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ops == nil {
		f.ops = make(map[uint64]context.CancelCauseFunc)
	}
	id := f.next
	f.next++
	f.ops[id] = cancel
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.ops, id)
	}
}

// interrupt makes every operation send its request again.
func (f *inflight) interrupt() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, cancel := range f.ops {
		cancel(errRetry)
	}
}
