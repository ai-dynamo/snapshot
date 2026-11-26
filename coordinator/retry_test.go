// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/internal/backoff"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
)

// testClient returns a client that has joined a session of the default
// limits, over a connection never made, which retries an operation that fails
// as the test makes it.
func testClient(t *testing.T) *client {
	s := &client{nc: &nats.Conn{}, closing: make(chan struct{}), done: make(chan struct{}), joined: true,
		budget: semaphore.NewWeighted(session.MaxPending(session.MaxValue)),
		slots:  semaphore.NewWeighted(session.MaxConcurrentGets)}
	s.life, s.endLife = context.WithCancel(context.Background())
	t.Cleanup(s.endLife)
	return s
}

// TestDoWaitsLongerEachTry covers an operation that fails transiently: a write
// the server drops because the stream's ingest queue is full, a watch refused
// at its bucket's consumer limit, and an answer the broken connection lost. do
// sends it again, each time after a longer wait, until it succeeds.
func TestDoWaitsLongerEachTry(t *testing.T) {
	for name, fail := range map[string]error{
		"a full ingest queue":   &jetstream.APIError{ErrorCode: jsTooManyRequests},
		"a bucket at its limit": &nats.APIError{ErrorCode: jsMaximumConsumers},
		"a broken connection":   errRetry,
	} {
		t.Run(name, func(t *testing.T) {
			s := testClient(t)
			var tries []time.Time
			err := s.do(context.Background(), func(context.Context) error {
				tries = append(tries, time.Now())
				if len(tries) <= 3 {
					return fail
				}
				return nil
			})
			require.NoError(t, err)
			require.Len(t, tries, 4)
			first := backoff.New().Duration
			for i, want := range []time.Duration{first, 2 * first, 4 * first} {
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

// TestWriteWaitsWhileReconnecting checks that a write is not sent while the
// client reconnects, even before nats.go has counted an attempt to reconnect.
// Such a send could wait on the connection through the next attempt's
// handshake, and go out over the new connection before its verification.
func TestWriteWaitsWhileReconnecting(t *testing.T) {
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: natsserver.RANDOM_PORT,
		NoLog: true, NoSigs: true})
	require.NoError(t, err)
	ns.Start()
	require.True(t, ns.ReadyForConnections(10*time.Second), "the server did not start")
	// The client tries to reconnect only after an hour, so it counts no
	// attempt while the test runs.
	nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(time.Hour),
		nats.ReconnectBufSize(-1))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	ns.Shutdown()
	ns.WaitForShutdown()
	require.Eventually(t, nc.IsReconnecting, 10*time.Second, 5*time.Millisecond)

	s := testClient(t)
	s.nc, s.wake = nc, make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	sent := false
	err = s.write(ctx, func(context.Context) error {
		sent = true
		return nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, sent, "the write was sent while the client reconnected")
}
