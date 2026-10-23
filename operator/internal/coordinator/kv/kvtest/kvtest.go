// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package kvtest is the conformance suite every kv backend must pass. A backend
// runs it from its own tests:
//
//	func TestConformance(t *testing.T) {
//		kvtest.Run(t, func(*testing.T) kv.Store { return memory.New() })
//	}
//
// The suite is deterministic: it never sleeps, and it observes a barrier
// without blocking by awaiting it under a context that is already done, which
// a backend answers at once for a barrier that has released.
package kvtest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

// The attempts and names the suite uses.
const (
	attempt  = types.UID("attempt-1")
	other    = types.UID("attempt-2")
	key      = "key"
	barrier  = "barrier"
	waiters  = 16
	writers  = 32
	deadline = 10 * time.Second
)

// Run runs the whole suite against the stores newStore builds, one empty store
// for each test.
func Run(t *testing.T, newStore func(t *testing.T) kv.Store) {
	tests := []struct {
		name string
		run  func(*testing.T, func(*testing.T) kv.Store)
	}{
		{"InsertIsWriteOnce", insertIsWriteOnce},
		{"ValuesAreCopies", valuesAreCopies},
		{"GetReportsAnAbsentKey", getReportsAnAbsentKey},
		{"UnsetIsIdempotent", unsetIsIdempotent},
		{"AddCountsAnAbsentKeyAsZero", addCountsAnAbsentKeyAsZero},
		{"AddUsesTheCounterEncoding", addUsesTheCounterEncoding},
		{"AddRejectsAValueThatIsNotACounter", addRejectsANonCounter},
		{"CompareAndSwap", compareAndSwap},
		{"WaitReturnsAnExistingValueAtOnce", waitReturnsAnExistingValueAtOnce},
		{"WaitIsReleasedByInsert", waitIsReleasedByInsert},
		{"WaitIsReleasedByAddCreatingAKey", waitIsReleasedByAdd},
		{"WaitReturnsWhenTheContextIsDone", waitReturnsOnCancellation},
		{"WaitReleasesEveryWaiter", waitReleasesEveryWaiter},
		{"CreateBarrierFixesTheCount", createBarrierFixesTheCount},
		{"BarrierReleasesExactlyAtTheCount", barrierReleasesExactlyAtTheCount},
		{"BarrierAcceptsPostsBeforeItIsCreated", barrierAcceptsPostsBeforeCreation},
		{"BarrierCountsAParticipantOnce", barrierCountsAParticipantOnce},
		{"BarrierMayBeAwaitedBeforeItIsCreated", barrierAwaitedBeforeCreation},
		{"BarrierStaysReleased", barrierStaysReleased},
		{"AwaitBarrierReturnsWhenTheContextIsDone", awaitBarrierReturnsOnCancellation},
		{"BarrierReleasesEveryWaiter", barrierReleasesEveryWaiter},
		{"KeysAndBarriersAreSeparate", keysAndBarriersAreSeparate},
		{"ConcurrentAddsApplyExactlyOnce", concurrentAddsApplyOnce},
		{"ConcurrentCompareAndSwapAdmitsOneWinner", concurrentCompareAndSwap},
		{"ConcurrentPostersReleaseTheBarrier", concurrentPosters},
		{"SpacesAreIsolated", spacesAreIsolated},
		{"DropDiscardsTheSpace", dropDiscardsTheSpace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, newStore) })
	}
}

func insertIsWriteOnce(t *testing.T, newStore func(*testing.T) kv.Store) {
	tests := []struct {
		name   string
		held   []byte
		insert []byte
		want   error
	}{
		{name: "absent key", insert: []byte("first")},
		{name: "same value again", held: []byte("first"), insert: []byte("first")},
		{name: "different value", held: []byte("first"), insert: []byte("second"), want: kv.ErrExists},
		{name: "empty value again", held: []byte{}, insert: []byte{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp, ctx := newStore(t).Space(attempt), context.Background()
			if tt.held != nil {
				require.NoError(t, sp.Insert(ctx, key, tt.held))
			}

			err := sp.Insert(ctx, key, tt.insert)

			want := tt.insert
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				want = tt.held
			} else {
				require.NoError(t, err)
			}
			got, err := sp.Get(ctx, key)
			require.NoError(t, err)
			// Compared as strings, since a backend may return an empty value
			// as nil.
			assert.Equal(t, string(want), string(got), "a refused insert must not overwrite")
		})
	}
}

func valuesAreCopies(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	value := []byte("handle")
	require.NoError(t, sp.Insert(ctx, key, value))

	value[0] = 'H'
	got, err := sp.Get(ctx, key)
	require.NoError(t, err)
	got[1] = 'A'
	again, err := sp.Get(ctx, key)

	require.NoError(t, err)
	assert.Equal(t, []byte("handle"), again, "neither the caller's buffer nor a returned value aliases the stored one")
}

func getReportsAnAbsentKey(t *testing.T, newStore func(*testing.T) kv.Store) {
	_, err := newStore(t).Space(attempt).Get(context.Background(), key)

	require.ErrorIs(t, err, kv.ErrNotFound)
}

func unsetIsIdempotent(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	require.NoError(t, sp.Insert(ctx, key, []byte("handle")))

	require.NoError(t, sp.Unset(ctx, key))
	require.NoError(t, sp.Unset(ctx, key))

	_, err := sp.Get(ctx, key)
	require.ErrorIs(t, err, kv.ErrNotFound)
	require.NoError(t, sp.Insert(ctx, key, []byte("again")), "an unset key may be inserted afresh")
}

func addCountsAnAbsentKeyAsZero(t *testing.T, newStore func(*testing.T) kv.Store) {
	for _, delta := range []int64{1, -1} {
		t.Run(fmt.Sprint(delta), func(t *testing.T) {
			sp, ctx := newStore(t).Space(attempt), context.Background()

			got, err := sp.Add(ctx, key, delta)

			require.NoError(t, err)
			assert.Equal(t, delta, got)
			assert.Equal(t, delta, counter(t, sp, key))
		})
	}
}

func addUsesTheCounterEncoding(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	require.NoError(t, sp.Insert(ctx, key, kv.EncodeCounter(41)))

	got, err := sp.Add(ctx, key, 1)

	require.NoError(t, err)
	assert.Equal(t, int64(42), got)
	value, err := sp.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, kv.EncodeCounter(42), value)
}

func addRejectsANonCounter(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	require.NoError(t, sp.Insert(ctx, key, []byte("seven")))

	_, err := sp.Add(ctx, key, 1)

	require.ErrorIs(t, err, kv.ErrNotCounter)
}

func compareAndSwap(t *testing.T, newStore func(*testing.T) kv.Store) {
	tests := []struct {
		name     string
		held     []byte
		expected []byte
		want     error
	}{
		{name: "absent key", expected: []byte("old"), want: kv.ErrNotFound},
		{name: "another value", held: []byte("other"), expected: []byte("old"), want: kv.ErrMismatch},
		{name: "the expected value", held: []byte("old"), expected: []byte("old")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp, ctx := newStore(t).Space(attempt), context.Background()
			if tt.held != nil {
				require.NoError(t, sp.Insert(ctx, key, tt.held))
			}

			err := sp.CompareAndSwap(ctx, key, tt.expected, []byte("new"))

			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
			value, err := sp.Get(ctx, key)
			require.NoError(t, err)
			assert.Equal(t, []byte("new"), value)
		})
	}
}

func waitReturnsAnExistingValueAtOnce(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp := newStore(t).Space(attempt)
	require.NoError(t, sp.Insert(context.Background(), key, []byte("handle")))

	value, err := sp.Wait(done(), key)

	require.NoError(t, err, "an existing key is returned even under a done context")
	assert.Equal(t, []byte("handle"), value)
}

func waitIsReleasedByInsert(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp := newStore(t).Space(attempt)
	got := waitAsync(sp, key)

	require.NoError(t, sp.Insert(context.Background(), key, []byte("handle")))

	result := await(t, got)
	require.NoError(t, result.err)
	assert.Equal(t, []byte("handle"), result.value)
}

func waitIsReleasedByAdd(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp := newStore(t).Space(attempt)
	got := waitAsync(sp, key)

	_, err := sp.Add(context.Background(), key, 1)
	require.NoError(t, err)

	result := await(t, got)
	require.NoError(t, result.err)
	assert.Equal(t, kv.EncodeCounter(1), result.value)
}

func waitReturnsOnCancellation(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp := newStore(t).Space(attempt)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := sp.Wait(ctx, key)
		got <- err
	}()

	cancel()

	require.ErrorIs(t, await(t, got), context.Canceled)
}

func waitReleasesEveryWaiter(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp := newStore(t).Space(attempt)
	results := make([]<-chan waitResult, 0, waiters)
	for range waiters {
		results = append(results, waitAsync(sp, key))
	}

	require.NoError(t, sp.Insert(context.Background(), key, []byte("handle")))

	for _, got := range results {
		result := await(t, got)
		require.NoError(t, result.err)
		assert.Equal(t, []byte("handle"), result.value)
	}
}

func createBarrierFixesTheCount(t *testing.T, newStore func(*testing.T) kv.Store) {
	tests := []struct {
		name  string
		again int
		want  error
	}{
		{name: "the same count", again: 2},
		{name: "a different count", again: 3, want: kv.ErrBarrierCount},
		{name: "a count of zero", again: 0, want: kv.ErrBarrierCount},
		{name: "a negative count", again: -1, want: kv.ErrBarrierCount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp, ctx := newStore(t).Space(attempt), context.Background()
			require.NoError(t, sp.CreateBarrier(ctx, barrier, 2))

			err := sp.CreateBarrier(ctx, barrier, tt.again)

			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
		})
	}
}

func barrierReleasesExactlyAtTheCount(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	require.NoError(t, sp.CreateBarrier(ctx, barrier, 3))
	got := awaitAsync(sp, barrier)

	require.NoError(t, sp.PostBarrier(ctx, barrier, "a"))
	require.NoError(t, sp.PostBarrier(ctx, barrier, "b"))
	assert.False(t, released(sp, barrier), "two of three participants must not release the barrier")

	require.NoError(t, sp.PostBarrier(ctx, barrier, "c"))

	require.NoError(t, await(t, got))
	assert.True(t, released(sp, barrier))
}

func barrierAcceptsPostsBeforeCreation(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	require.NoError(t, sp.PostBarrier(ctx, barrier, "a"))
	require.NoError(t, sp.PostBarrier(ctx, barrier, "b"))
	assert.False(t, released(sp, barrier), "a barrier nobody created yet cannot release")

	require.NoError(t, sp.CreateBarrier(ctx, barrier, 2))

	assert.True(t, released(sp, barrier))
}

func barrierCountsAParticipantOnce(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	require.NoError(t, sp.CreateBarrier(ctx, barrier, 2))

	require.NoError(t, sp.PostBarrier(ctx, barrier, "a"))
	require.NoError(t, sp.PostBarrier(ctx, barrier, "a"))

	assert.False(t, released(sp, barrier), "one participant posting twice counts once")
}

func barrierAwaitedBeforeCreation(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	got := awaitAsync(sp, barrier)

	require.NoError(t, sp.PostBarrier(ctx, barrier, "a"))
	require.NoError(t, sp.CreateBarrier(ctx, barrier, 1))

	require.NoError(t, await(t, got))
}

func barrierStaysReleased(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	require.NoError(t, sp.CreateBarrier(ctx, barrier, 1))
	require.NoError(t, sp.PostBarrier(ctx, barrier, "a"))

	require.NoError(t, sp.PostBarrier(ctx, barrier, "b"))
	require.NoError(t, sp.CreateBarrier(ctx, barrier, 1))

	assert.True(t, released(sp, barrier), "a released barrier stays released")
}

func awaitBarrierReturnsOnCancellation(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp := newStore(t).Space(attempt)
	require.NoError(t, sp.CreateBarrier(context.Background(), barrier, 2))
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { got <- sp.AwaitBarrier(ctx, barrier) }()

	cancel()

	require.ErrorIs(t, await(t, got), context.Canceled)
}

func barrierReleasesEveryWaiter(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	require.NoError(t, sp.CreateBarrier(ctx, barrier, 1))
	results := make([]<-chan error, 0, waiters)
	for range waiters {
		results = append(results, awaitAsync(sp, barrier))
	}

	require.NoError(t, sp.PostBarrier(ctx, barrier, "a"))

	for _, got := range results {
		require.NoError(t, await(t, got))
	}
}

func keysAndBarriersAreSeparate(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp, ctx := newStore(t).Space(attempt), context.Background()
	require.NoError(t, sp.Insert(ctx, barrier, []byte("a value, not a barrier")))

	require.NoError(t, sp.CreateBarrier(ctx, barrier, 1))
	require.NoError(t, sp.PostBarrier(ctx, barrier, "a"))

	assert.True(t, released(sp, barrier))
	value, err := sp.Get(ctx, barrier)
	require.NoError(t, err)
	assert.Equal(t, []byte("a value, not a barrier"), value)
}

func concurrentAddsApplyOnce(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp := newStore(t).Space(attempt)
	seen := make(chan int64, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			value, err := sp.Add(context.Background(), key, 1)
			assert.NoError(t, err)
			seen <- value
		})
	}
	wg.Wait()
	close(seen)

	got := make([]int64, 0, writers)
	for value := range seen {
		got = append(got, value)
	}
	slices.Sort(got)
	want := make([]int64, 0, writers)
	for i := range int64(writers) {
		want = append(want, i+1)
	}
	assert.Equal(t, want, got, "every add must see its own new value")
	assert.Equal(t, int64(writers), counter(t, sp, key))
}

func concurrentCompareAndSwap(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp := newStore(t).Space(attempt)
	require.NoError(t, sp.Insert(context.Background(), key, []byte("open")))
	var winners atomic.Int64
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			err := sp.CompareAndSwap(context.Background(), key, []byte("open"), fmt.Appendf(nil, "taken-%d", i))
			if err == nil {
				winners.Add(1)
				return
			}
			assert.ErrorIs(t, err, kv.ErrMismatch)
		})
	}
	wg.Wait()

	assert.Equal(t, int64(1), winners.Load())
}

func concurrentPosters(t *testing.T, newStore func(*testing.T) kv.Store) {
	sp := newStore(t).Space(attempt)
	require.NoError(t, sp.CreateBarrier(context.Background(), barrier, writers))
	got := awaitAsync(sp, barrier)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			// Every participant posts twice, so the count holds only if
			// duplicates are recognized under contention too.
			for range 2 {
				assert.NoError(t, sp.PostBarrier(context.Background(), barrier, fmt.Sprintf("p-%d", i)))
			}
		})
	}
	wg.Wait()

	require.NoError(t, await(t, got))
}

func spacesAreIsolated(t *testing.T, newStore func(*testing.T) kv.Store) {
	store, ctx := newStore(t), context.Background()
	first, second := store.Space(attempt), store.Space(other)

	require.NoError(t, first.Insert(ctx, key, []byte("first")))
	require.NoError(t, second.Insert(ctx, key, []byte("second")))
	require.NoError(t, first.CreateBarrier(ctx, barrier, 1))
	require.NoError(t, first.PostBarrier(ctx, barrier, "a"))

	firstValue, err := first.Get(ctx, key)
	require.NoError(t, err)
	secondValue, err := second.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), firstValue)
	assert.Equal(t, []byte("second"), secondValue)
	assert.False(t, released(second, barrier), "a barrier of one space is invisible to another")
}

func dropDiscardsTheSpace(t *testing.T, newStore func(*testing.T) kv.Store) {
	store, ctx := newStore(t), context.Background()
	require.NoError(t, store.Space(attempt).Insert(ctx, key, []byte("dropped")))
	require.NoError(t, store.Space(attempt).CreateBarrier(ctx, barrier, 1))
	require.NoError(t, store.Space(attempt).PostBarrier(ctx, barrier, "a"))
	require.NoError(t, store.Space(other).Insert(ctx, key, []byte("kept")))

	require.NoError(t, store.Drop(ctx, attempt))
	require.NoError(t, store.Drop(ctx, attempt), "dropping a space twice succeeds")
	require.NoError(t, store.Drop(ctx, "never-created"))

	_, err := store.Space(attempt).Get(ctx, key)
	require.ErrorIs(t, err, kv.ErrNotFound)
	assert.False(t, released(store.Space(attempt), barrier), "a dropped space starts again empty")
	kept, err := store.Space(other).Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("kept"), kept)
}

// waitResult is what a Wait returned.
type waitResult struct {
	value []byte
	err   error
}

// waitAsync starts a Wait and returns where its result arrives.
func waitAsync(sp kv.Space, key string) <-chan waitResult {
	got := make(chan waitResult, 1)
	go func() {
		value, err := sp.Wait(context.Background(), key)
		got <- waitResult{value: value, err: err}
	}()
	return got
}

// awaitAsync starts an AwaitBarrier and returns where its result arrives.
func awaitAsync(sp kv.Space, name string) <-chan error {
	got := make(chan error, 1)
	go func() { got <- sp.AwaitBarrier(context.Background(), name) }()
	return got
}

// released reports whether a barrier has released, without blocking: a backend
// answers at once for a released barrier, even under a done context.
func released(sp kv.Space, name string) bool {
	return sp.AwaitBarrier(done(), name) == nil
}

// done returns a context that is already done.
func done() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// counter decodes the counter held under key.
func counter(t *testing.T, sp kv.Space, key string) int64 {
	t.Helper()
	value, err := sp.Get(context.Background(), key)
	require.NoError(t, err)
	n, err := kv.DecodeCounter(value)
	require.NoError(t, err)
	return n
}

// await returns what a blocking call reported, failing the test if it never
// returns.
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(deadline):
		t.Fatal("a blocking call never returned")
		panic("unreachable")
	}
}
