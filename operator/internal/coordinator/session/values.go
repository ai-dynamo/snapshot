// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
)

// errWaitExpired is the cause a Get's own timer cancels its wait with, which is
// how the wait expiring is told apart from the caller giving up.
var errWaitExpired = errors.New("wait expired")

// Insert stores a value if the key is absent. Inserting the value a key already
// holds succeeds, so a retry is safe even without a request identifier; a
// different value reports ErrExists.
func (s *Session) Insert(ctx context.Context, r Request, key string, value []byte) error {
	if err := r.Caller.authorize(key, value); err != nil {
		return err
	}
	_, err := s.mutate(ctx, r, OpInsert, func(ctx context.Context, sp kv.Space) (int64, error) {
		return 0, sp.Insert(ctx, key, value)
	})
	return err
}

// Unset removes a key, whether or not it was set, so a retry is safe.
func (s *Session) Unset(ctx context.Context, r Request, key string) error {
	if err := r.Caller.authorize(key); err != nil {
		return err
	}
	_, err := s.mutate(ctx, r, OpUnset, func(ctx context.Context, sp kv.Space) (int64, error) {
		return 0, sp.Unset(ctx, key)
	})
	return err
}

// Increment adds one to the counter at key and returns the new value. An absent
// key counts as zero, and a value that is not a counter reports ErrNotCounter.
func (s *Session) Increment(ctx context.Context, r Request, key string) (int64, error) {
	return s.add(ctx, r, OpIncrement, key, 1)
}

// Decrement takes one from the counter at key and returns the new value, on the
// same terms as Increment.
func (s *Session) Decrement(ctx context.Context, r Request, key string) (int64, error) {
	return s.add(ctx, r, OpDecrement, key, -1)
}

// CompareAndSwap replaces the value at key only if it is the one expected. It
// reports ErrNotFound when the key is absent and ErrMismatch when it holds
// something else.
func (s *Session) CompareAndSwap(ctx context.Context, r Request, key string, expected, value []byte) error {
	if err := r.Caller.authorize(key, expected, value); err != nil {
		return err
	}
	_, err := s.mutate(ctx, r, OpCompareAndSwap, func(ctx context.Context, sp kv.Space) (int64, error) {
		return 0, sp.CompareAndSwap(ctx, key, expected, value)
	})
	return err
}

// Get returns the value at key, waiting up to wait for it to appear. A wait of
// zero reports ErrNotFound at once, an expired wait reports ErrNotYet, and a
// session that ends while the caller waits reports how it ended. The call
// returns as soon as ctx is done. A Get with a wait reports its time through
// Observer.KeyWaited rather than as an operation latency.
func (s *Session) Get(ctx context.Context, c Caller, key string, wait time.Duration) ([]byte, error) {
	if err := c.authorize(key); err != nil {
		return nil, err
	}
	if wait > 0 {
		return s.await(ctx, c, key, wait)
	}
	defer s.observeOperation(c.Role, OpGet, s.clock.Now())
	var value []byte
	err := s.call(ctx, c, func(ctx context.Context, sp kv.Space) (err error) {
		value, err = sp.Get(ctx, key)
		return err
	})
	return value, s.settle(ctx, err)
}

// await is Get with a wait. It waits under a context that the wait's own timer
// also cancels, and reports ErrNotYet when that timer is what stopped it. The
// timer is armed before the call enters the space and stopped after it leaves,
// since neither may happen while the call is counted in flight.
func (s *Session) await(ctx context.Context, c Caller, key string, wait time.Duration) ([]byte, error) {
	start := s.clock.Now()
	waitCtx, expire := context.WithCancelCause(ctx)
	timer := s.clock.AfterFunc(wait, func() { expire(errWaitExpired) })
	var value []byte
	err := s.call(waitCtx, c, func(ctx context.Context, sp kv.Space) (err error) {
		value, err = sp.Wait(ctx, key)
		return err
	})
	timer.Stop()
	expire(nil)
	s.observer.KeyWaited(s.uid, c.Role, s.clock.Since(start))

	if err != nil && s.ctx.Err() == nil && errors.Is(context.Cause(waitCtx), errWaitExpired) {
		return nil, fmt.Errorf("key %q: %w", key, ErrNotYet)
	}
	return value, s.settle(ctx, err)
}

// add applies delta to a counter and returns the new value.
func (s *Session) add(ctx context.Context, r Request, op Operation, key string, delta int64) (int64, error) {
	if err := r.Caller.authorize(key); err != nil {
		return 0, err
	}
	return s.mutate(ctx, r, op, func(ctx context.Context, sp kv.Space) (int64, error) {
		return sp.Add(ctx, key, delta)
	})
}
