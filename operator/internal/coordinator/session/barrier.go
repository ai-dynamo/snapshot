// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
)

// CreateBarrier fixes how many distinct participants a barrier waits for.
// Creating it again with the same count succeeds, so a retry is safe; a
// different count, or a count below one, reports ErrBarrierCount. A barrier
// releases once and stays released, which is what Snapshot's own barriers
// need, since each is passed once per attempt.
func (s *Session) CreateBarrier(ctx context.Context, r Request, name string, count int) error {
	if err := r.Caller.authorize(name); err != nil {
		return err
	}
	_, err := s.mutate(ctx, r, OpCreateBarrier, func(ctx context.Context, sp kv.Space) (int64, error) {
		return 0, sp.CreateBarrier(ctx, name, count)
	})
	return err
}

// PostBarrier records one participant, releasing the barrier once it has as
// many as it was created with. An agent and a relay post as their own member,
// and any other participant is refused, so a barrier counting members counts
// each member once. A library posts under the identity of its process, because
// several processes of one member post to the same barrier, and the barrier
// counts it as the pair of its member and that identity, so processes of
// different members that pick the same identity count apart. Posting twice
// counts once, and posting before the barrier is created is valid.
func (s *Session) PostBarrier(ctx context.Context, r Request, name string, participant ParticipantID) error {
	if err := r.Caller.authorize(name); err != nil {
		return err
	}
	if err := r.Caller.authorizePost(participant); err != nil {
		return err
	}
	scoped := r.Caller.scope(participant)
	_, err := s.mutate(ctx, r, OpPostBarrier, func(ctx context.Context, sp kv.Space) (int64, error) {
		return 0, sp.PostBarrier(ctx, name, scoped)
	})
	return err
}

// AwaitBarrier returns once the barrier has as many distinct participants as it
// was created with. Waiting before it is created is valid. A session that ends
// while the caller waits reports how it ended, and the call returns as soon as
// ctx is done. It reports its time through Observer.BarrierWaited.
func (s *Session) AwaitBarrier(ctx context.Context, c Caller, name string) error {
	if err := c.authorize(name); err != nil {
		return err
	}
	start := s.clock.Now()
	err := s.call(ctx, c, func(ctx context.Context, sp kv.Space) error {
		return sp.AwaitBarrier(ctx, name)
	})
	s.observer.BarrierWaited(s.uid, c.Role, s.clock.Since(start))
	return s.settle(ctx, err)
}
