// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageErrorsReachCallersAsSessionSentinels(t *testing.T) {
	f := newFixture(t)
	ctx, r := context.Background(), requestOf(memberA, "")
	require.NoError(t, f.session.Insert(ctx, r, agentKey, []byte("first")))
	require.NoError(t, f.session.Insert(ctx, r, agentOther, []byte("seven")))

	_, notFound := f.session.Get(ctx, agentOf(memberA), agentNamespace+"absent", 0)
	exists := f.session.Insert(ctx, r, agentKey, []byte("second"))
	mismatch := f.session.CompareAndSwap(ctx, r, agentKey, []byte("other"), []byte("new"))
	_, notCounter := f.session.Increment(ctx, r, agentOther)
	barrierCount := f.session.CreateBarrier(ctx, r, barrierName, 0)

	require.ErrorIs(t, notFound, ErrNotFound)
	require.ErrorIs(t, exists, ErrExists)
	require.ErrorIs(t, mismatch, ErrMismatch)
	require.ErrorIs(t, notCounter, ErrNotCounter)
	require.ErrorIs(t, barrierCount, ErrBarrierCount)
	require.NoError(t, f.session.Insert(ctx, r, agentKey, []byte("first")), "re-inserting the same value is a retry")
}

func TestGetCompletesWhenTheKeyIsInserted(t *testing.T) {
	f := newFixture(t)
	type result struct {
		value []byte
		err   error
	}
	got := make(chan result, 1)
	go func() {
		value, err := f.session.Get(context.Background(), agentOf(memberB), agentKey, deadlineIn)
		got <- result{value: value, err: err}
	}()
	f.clock.AwaitTimer(t)

	require.NoError(t, f.session.Insert(context.Background(), requestOf(memberA, ""), agentKey, []byte("handle")))

	r := await(t, got)
	require.NoError(t, r.err)
	assert.Equal(t, []byte("handle"), r.value)
}

func TestGetReportsNotYetWhenTheWaitExpires(t *testing.T) {
	f := newFixture(t)
	read := getAsync(t, f, agentOf(memberA), agentKey, 5*time.Second)

	f.clock.Step(5 * time.Second)

	require.ErrorIs(t, await(t, read), ErrNotYet)
}

func TestGetReturnsWhenTheContextIsCancelled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := f.session.Get(ctx, agentOf(memberA), agentKey, deadlineIn)
		got <- err
	}()
	f.clock.AwaitTimer(t)

	cancel()

	err := await(t, got)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrNotYet, "the caller giving up is not the wait expiring")
}

func TestGetWakesEveryWaiterOnOneKey(t *testing.T) {
	// The bubble lets the insert wait until every Get is durably blocked, so
	// each one is woken rather than finding the key already there.
	synctest.Test(t, func(t *testing.T) {
		const waiters = 16
		f := newFixture(t)
		values := make(chan []byte, waiters)
		var wg sync.WaitGroup
		for range waiters {
			wg.Go(func() {
				value, err := f.session.Get(context.Background(), agentOf(memberB), agentKey, deadlineIn)
				assert.NoError(t, err)
				values <- value
			})
		}
		synctest.Wait()

		require.NoError(t, f.session.Insert(context.Background(), requestOf(memberA, ""), agentKey, []byte("handle")))
		wg.Wait()
		close(values)

		for value := range values {
			assert.Equal(t, []byte("handle"), value)
		}
	})
}

func TestCountersCountAnAbsentKeyAsZero(t *testing.T) {
	f := newFixture(t)
	ctx, r := context.Background(), requestOf(memberA, "")

	up, err := f.session.Increment(ctx, r, agentKey)
	require.NoError(t, err)
	down, err := f.session.Decrement(ctx, r, agentOther)
	require.NoError(t, err)

	assert.Equal(t, int64(1), up)
	assert.Equal(t, int64(-1), down)
	stored, err := f.stored(attemptUID, agentOther)
	require.NoError(t, err)
	assert.Equal(t, kv.EncodeCounter(-1), stored)
}

func TestConcurrentIncrementsApplyExactlyOnce(t *testing.T) {
	const callers = 32
	f := newFixture(t)
	seen := make(chan int64, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			value, err := f.session.Increment(context.Background(), requestOf(memberA, ""), agentKey)
			assert.NoError(t, err)
			seen <- value
		})
	}
	wg.Wait()
	close(seen)

	got := make([]int64, 0, callers)
	for value := range seen {
		got = append(got, value)
	}
	slices.Sort(got)
	want := make([]int64, 0, callers)
	for i := range int64(callers) {
		want = append(want, i+1)
	}
	assert.Equal(t, want, got, "every increment must see its own new value")
}

func TestConcurrentCompareAndSwapAdmitsOneWinner(t *testing.T) {
	const callers = 32
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.session.Insert(ctx, requestOf(memberA, ""), agentKey, []byte("open")))
	var winners atomic.Int64
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			err := f.session.CompareAndSwap(ctx, requestOf(memberA, ""), agentKey, []byte("open"),
				fmt.Appendf(nil, "taken-%d", i))
			if err == nil {
				winners.Add(1)
				return
			}
			assert.ErrorIs(t, err, ErrMismatch)
		})
	}
	wg.Wait()

	assert.Equal(t, int64(1), winners.Load())
}

func TestRetriedRequestsReplayTheirOutcome(t *testing.T) {
	const retried = RequestID("request-1")
	tests := []struct {
		name  string
		apply func(*Session, Request) (int64, error)
		want  int64
	}{
		{name: "increment", apply: func(s *Session, r Request) (int64, error) {
			return s.Increment(context.Background(), r, agentKey)
		}, want: 1},
		{name: "decrement", apply: func(s *Session, r Request) (int64, error) {
			return s.Decrement(context.Background(), r, agentKey)
		}, want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			r := requestOf(memberA, retried)

			first, err := tt.apply(f.session, r)
			require.NoError(t, err)
			again, err := tt.apply(f.session, r)
			require.NoError(t, err)

			assert.Equal(t, tt.want, first)
			assert.Equal(t, tt.want, again)
			stored, err := f.stored(attemptUID, agentKey)
			require.NoError(t, err)
			assert.Equal(t, kv.EncodeCounter(tt.want), stored, "a retried request applies once")
		})
	}
}

func TestARetryReplaysTheFailureWithoutWhatItNamed(t *testing.T) {
	const retried = RequestID("request-1")
	f := newFixture(t)
	ctx := context.Background()
	require.NoError(t, f.session.Insert(ctx, requestOf(memberA, ""), agentKey, []byte("first")))

	first := f.session.Insert(ctx, requestOf(memberA, retried), agentKey, []byte("second"))
	replayed := f.session.Insert(ctx, requestOf(memberA, retried), agentKey, []byte("third"))

	require.ErrorIs(t, first, ErrExists)
	assert.Contains(t, first.Error(), agentKey, "the caller who made the request learns which key")
	require.ErrorIs(t, replayed, ErrExists, "a retry reports the outcome that was recorded, not a fresh one")
	assert.NotContains(t, replayed.Error(), agentKey, "a record keeps the error's identity, never the key")
}

func TestARetryRunsAgainAfterAFailureWithNoIdentity(t *testing.T) {
	const retried = RequestID("request-1")
	f := newManagerFixtureOn(t, &flakyStore{Store: memory.New(), failures: 1})
	s := f.ensure(t, specOf(attemptUID))
	ctx, r := context.Background(), requestOf(memberA, retried)

	require.ErrorIs(t, s.Insert(ctx, r, agentKey, []byte("handle")), errBackendDown)
	require.NoError(t, s.Insert(ctx, r, agentKey, []byte("handle")), "an unrecorded failure is not replayed")

	stored, err := f.stored(attemptUID, agentKey)
	require.NoError(t, err)
	assert.Equal(t, []byte("handle"), stored)
}

func TestConcurrentRetriesOfOneRequestApplyOnce(t *testing.T) {
	const retries = 32
	f := newFixture(t)
	seen := make(chan int64, retries)
	var wg sync.WaitGroup
	for range retries {
		wg.Go(func() {
			value, err := f.session.Increment(context.Background(), requestOf(memberA, "request-1"), agentKey)
			assert.NoError(t, err)
			seen <- value
		})
	}
	wg.Wait()
	close(seen)

	for value := range seen {
		assert.Equal(t, int64(1), value, "a retry that arrives while the original runs waits for its outcome")
	}
	stored, err := f.stored(attemptUID, agentKey)
	require.NoError(t, err)
	assert.Equal(t, kv.EncodeCounter(1), stored)
}

func TestRequestIDsAreScopedToOneCaller(t *testing.T) {
	const shared = RequestID("request-1")
	cudart := Caller{Member: memberA, Role: RoleLibrary, Library: "cudart", Version: "13.0"}
	tests := []struct {
		name          string
		first, second Caller
		firstKey      string
		secondKey     string
		// want is the second key's counter once both increments apply.
		want int64
	}{
		{
			name:  "two members",
			first: agentOf(memberA), second: agentOf(memberB),
			firstKey: agentKey, secondKey: agentKey,
			want: 2,
		},
		{
			name:  "an agent and the relay of its member",
			first: relayOf(memberA), second: agentOf(memberA),
			firstKey: relayKey, secondKey: agentKey,
			want: 1,
		},
		{
			name:  "two libraries of one member",
			first: ncclOf(memberA), second: cudart,
			firstKey: ncclKey, secondKey: "cuda/cudart/13.0/counter",
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()

			_, err := f.session.Increment(ctx, Request{Caller: tt.first, ID: shared}, tt.firstKey)
			require.NoError(t, err)
			_, err = f.session.Increment(ctx, Request{Caller: tt.second, ID: shared}, tt.secondKey)
			require.NoError(t, err)

			stored, err := f.stored(attemptUID, tt.secondKey)
			require.NoError(t, err, "the second caller's increment must apply, not replay the first's")
			assert.Equal(t, kv.EncodeCounter(tt.want), stored)
		})
	}
}

func TestAnEmptyRequestIDAppliesEveryTime(t *testing.T) {
	f := newFixture(t)
	ctx, r := context.Background(), requestOf(memberA, "")

	_, err := f.session.Increment(ctx, r, agentKey)
	require.NoError(t, err)
	_, err = f.session.Increment(ctx, r, agentKey)
	require.NoError(t, err)

	stored, err := f.stored(attemptUID, agentKey)
	require.NoError(t, err)
	assert.Equal(t, kv.EncodeCounter(2), stored)
}

func TestAWriteCompletesWhenItsCallerHasGivenUp(t *testing.T) {
	// A backend that honors cancellation, since the memory backend ignores
	// contexts and would apply the write whether or not the session shields it
	// from its caller.
	f := newManagerFixtureOn(t, honoringStore{memory.New()})
	s := f.ensure(t, specOf(attemptUID))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Insert(ctx, requestOf(memberA, "request-1"), agentKey, []byte("handle"))

	require.NoError(t, err, "a write runs to completion, so the outcome a retry replays is the real one")
	stored, err := f.stored(attemptUID, agentKey)
	require.NoError(t, err)
	assert.Equal(t, []byte("handle"), stored)
}
