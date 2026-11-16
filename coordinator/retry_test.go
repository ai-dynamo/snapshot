// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
)

// testClient returns a client that has joined a session of the default
// limits, with no connection, which retries an operation that fails as the
// test makes it.
func testClient(t *testing.T) *client {
	s := &client{closing: make(chan struct{}), done: make(chan struct{}), joined: true,
		budget: semaphore.NewWeighted(session.MaxPending(session.MaxValue))}
	s.life, s.endLife = context.WithCancel(context.Background())
	t.Cleanup(s.endLife)
	return s
}

// TestBackoff covers the waits between the resends of an operation the
// coordinator is too busy for: they double up to a cap, and start again after
// reset.
func TestBackoff(t *testing.T) {
	var b backoff
	var waits []time.Duration
	for range 6 {
		waits = append(waits, b.next())
	}
	assert.Equal(t, []time.Duration{retryWait, 2 * retryWait, 4 * retryWait, 8 * retryWait, maxRetryWait,
		maxRetryWait}, waits)
	b.reset()
	assert.Equal(t, retryWait, b.next())
}

// TestDoRetriesWhenTheCoordinatorIsBusy covers a write that the server drops
// because the stream's ingest queue is full, and a watch refused at its
// bucket's consumer limit: do sends the operation again, each time after a
// longer wait, until it succeeds.
func TestDoRetriesWhenTheCoordinatorIsBusy(t *testing.T) {
	for name, code := range map[string]int{
		"a full ingest queue":   jsTooManyRequests,
		"a bucket at its limit": jsMaximumConsumers,
	} {
		t.Run(name, func(t *testing.T) {
			s := testClient(t)
			var tries []time.Time
			err := s.do(context.Background(), func(context.Context) error {
				tries = append(tries, time.Now())
				if len(tries) <= 3 {
					return &nats.APIError{ErrorCode: nats.ErrorCode(code)}
				}
				return nil
			})
			require.NoError(t, err)
			require.Len(t, tries, 4)
			for i, want := range []time.Duration{retryWait, 2 * retryWait, 4 * retryWait} {
				assert.GreaterOrEqual(t, tries[i+1].Sub(tries[i]), want, "the wait before try %d", i+2)
			}
		})
	}
}

// TestDoStopsRetryingWhenContextEnds checks that a busy coordinator does not
// keep a caller whose context is done.
func TestDoStopsRetryingWhenContextEnds(t *testing.T) {
	s := testClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := s.do(ctx, func(context.Context) error { return &jetstream.APIError{ErrorCode: jsTooManyRequests} })
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestDoDoesNotRetryAFullBucket checks that an error that waiting does not
// cure is reported at once.
func TestDoDoesNotRetryAFullBucket(t *testing.T) {
	s := testClient(t)
	tries := 0
	err := s.do(context.Background(), func(context.Context) error {
		tries++
		return &jetstream.APIError{ErrorCode: jsStoreFailed}
	})
	require.ErrorIs(t, err, session.ErrTooMany)
	assert.Equal(t, 1, tries)
}
