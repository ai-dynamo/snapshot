// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/require"
)

// pending is how long a test watches a read to check that it has not begun.
const pending = 100 * time.Millisecond

// TestReadsShareABudgetInBytes checks that the reads of a connection share a
// budget, in bytes, of what the coordinator buffers for it: while
// session.MaxConcurrentGets reads of the Snapshot bucket's largest values are
// in flight, a read of the library bucket goes on, and one more read of the
// Snapshot bucket waits until one of them has ended.
func TestReadsShareABudgetInBytes(t *testing.T) {
	s := testClient(t)
	ctx := context.Background()
	large, small := readSize(session.MaxValue), readSize(session.MaxLibraryValue)
	started, release := make(chan struct{}), make(chan struct{})
	read := func(size int64) <-chan error {
		done := make(chan error, 1)
		go func() {
			done <- s.read(ctx, size, func(context.Context) error {
				started <- struct{}{}
				<-release
				return nil
			})
		}()
		return done
	}
	var reads []<-chan error
	for range session.MaxConcurrentGets {
		reads = append(reads, read(large))
		<-started
	}
	require.NoError(t, s.read(ctx, small, func(context.Context) error { return nil }),
		"a small read must go on beside the largest ones")

	reads = append(reads, read(large))
	select {
	case <-started:
		t.Fatal("a read past the budget must wait")
	case <-time.After(pending):
	}
	close(release)
	<-started
	for _, done := range reads {
		require.NoError(t, <-done)
	}
}
