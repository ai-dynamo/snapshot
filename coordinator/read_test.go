// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pending is how long a test watches a read to check that it has not begun.
const pending = 100 * time.Millisecond

// The costs of reads of the Snapshot and library buckets at their default
// limits.
var (
	largeRead = readCost{bytes: readSize(session.MaxValue), slot: true}
	smallRead = readCost{bytes: readSize(session.MaxLibraryValue)}
)

// TestReadsShareABudgetInBytes checks that the reads of a connection share a
// budget, in bytes, of what the coordinator buffers for it: while
// session.MaxConcurrentGets reads of the Snapshot bucket's largest values are
// in flight, one more waits, and a read of the library bucket goes on beside
// them, however long the waiting one waits; the waiting one runs once one of
// them has ended.
func TestReadsShareABudgetInBytes(t *testing.T) {
	s := testClient(t)
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	read := func(cost readCost) <-chan error {
		done := make(chan error, 1)
		go func() {
			done <- s.read(ctx, cost, func(context.Context) error {
				started <- struct{}{}
				<-release
				return nil
			})
		}()
		return done
	}
	var reads []<-chan error
	for range session.MaxConcurrentGets {
		reads = append(reads, read(largeRead))
		<-started
	}
	reads = append(reads, read(largeRead))
	select {
	case <-started:
		t.Fatal("a read past the budget must wait")
	case <-time.After(pending):
	}
	small := make(chan error, 1)
	go func() { small <- s.read(ctx, smallRead, func(context.Context) error { return nil }) }()
	select {
	case err := <-small:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("a small read must go on beside the largest ones, and the one waiting")
	}

	close(release)
	<-started
	for _, done := range reads {
		require.NoError(t, <-done)
	}
}

// TestSlowReadIsNotSentAgain checks that a read whose reply takes longer than
// attemptTimeout to arrive is sent once: a read sent again while its first
// reply is still buffered could take the connection past what the coordinator
// holds for it.
func TestSlowReadIsNotSentAgain(t *testing.T) {
	s := testClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*attemptTimeout)
	defer cancel()
	var tries atomic.Int32
	err := s.read(ctx, largeRead, func(ctx context.Context) error {
		tries.Add(1)
		select {
		case <-time.After(attemptTimeout + pending):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, tries.Load())
}

// TestReadKeepsItsBytesUntilItsReplyArrives checks that a read whose caller
// gives up returns at once, yet keeps its bytes of the connection's budget
// until its reply has arrived: until then, the reply may sit in what the
// coordinator buffers for the connection.
func TestReadKeepsItsBytesUntilItsReplyArrives(t *testing.T) {
	s := testClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	all := session.MaxPending(session.MaxValue)
	started, reply := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.read(ctx, readCost{bytes: all}, func(context.Context) error {
			close(started)
			<-reply
			return nil
		})
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.False(t, s.budget.TryAcquire(1), "the read must keep its bytes until its reply arrives")
	close(reply)
	require.Eventually(t, func() bool { return s.budget.TryAcquire(all) }, time.Second, time.Millisecond,
		"the reply must free the read's bytes")
}

// TestReadIsSentAgainOnceItsConnectionBreaks checks that a disconnect ends a
// read that waits for its reply, which the broken connection lost, and sends it
// again, once its bytes are free.
func TestReadIsSentAgainOnceItsConnectionBreaks(t *testing.T) {
	s := testClient(t)
	started := make(chan struct{}, 2)
	var tries atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- s.read(context.Background(), largeRead, func(ctx context.Context) error {
			started <- struct{}{}
			if tries.Add(1) == 1 {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		})
	}()
	<-started
	s.disconnected(s.nc)
	require.NoError(t, <-done)
	assert.EqualValues(t, 2, tries.Load())
}
