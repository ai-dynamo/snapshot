// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"sync"
)

// inflight tracks the operations waiting for the coordinator's answer, each
// with the number of the connection it was sent over. A request sent just
// before the connection broke is lost with it, so a disconnect makes every
// operation sent over that connection send its request again, which is safe
// for every operation, instead of waiting for an answer that never comes.
type inflight struct {
	mu   sync.Mutex
	next uint64
	ops  map[uint64]tracked
}

// tracked is one operation inflight tracks.
type tracked struct {
	// connection numbers the connection the operation was sent over.
	connection uint64
	cancel     context.CancelCauseFunc
}

// add tracks an operation sent over the connection numbered connection, which
// interrupt ends through cancel. The returned function stops tracking it.
func (f *inflight) add(connection uint64, cancel context.CancelCauseFunc) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ops == nil {
		f.ops = make(map[uint64]tracked)
	}
	id := f.next
	f.next++
	f.ops[id] = tracked{connection: connection, cancel: cancel}
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.ops, id)
	}
}

// interrupt makes every operation sent over a connection numbered below
// before send its request again.
func (f *inflight) interrupt(before uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range f.ops {
		if op.connection < before {
			op.cancel(errRetry)
		}
	}
}
