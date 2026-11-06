// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatortest_test

import (
	"context"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestStart starts a coordinator and ensures a session, whose members connect
// through Config, meet at a barrier, and know their ranks, and checks that a
// member cannot connect while the Authorizer is unavailable.
func TestStart(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := coordinatortest.Start(t)
	spec := server.Spec{UID: "attempt", Deadline: time.Now().Add(time.Hour),
		Members: []session.Member{{ID: "leader", Rank: 0}, {ID: "worker", Rank: 1}}}
	require.NoError(t, c.Ensure(ctx, spec))

	sessions := make([]coordinator.Session, len(spec.Members))
	for i, m := range spec.Members {
		s, err := coordinator.Connect(ctx, c.Config(spec.UID, m.ID))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		assert.Equal(t, m.Rank, s.Rank())
		sessions[i] = s
	}
	arrived := make(chan error, 1)
	go func() { arrived <- sessions[0].Barrier(ctx, "admitted") }()
	require.NoError(t, sessions[1].Barrier(ctx, "admitted"))
	require.NoError(t, <-arrived)

	c.Authorizer.Unavailable.Store(true)
	unavailable, cancelUnavailable := context.WithTimeout(ctx, time.Second)
	defer cancelUnavailable()
	_, err := coordinator.Connect(unavailable, c.Config(spec.UID, "worker"))
	require.ErrorIs(t, err, session.ErrForbidden)
}

// TestStartUnready starts a coordinator that refuses members until MarkReady.
func TestStartUnready(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := coordinatortest.StartUnready(t)
	spec := server.Spec{UID: "attempt", Deadline: time.Now().Add(time.Hour),
		Members: []session.Member{{ID: "member", Rank: 0}}}
	require.NoError(t, c.Ensure(ctx, spec))
	assert.False(t, c.Ready())
	refused, cancelRefused := context.WithTimeout(ctx, time.Second)
	defer cancelRefused()
	_, err := coordinator.Connect(refused, c.Config(spec.UID, "member"))
	require.ErrorIs(t, err, session.ErrForbidden)

	c.MarkReady()
	s, err := coordinator.Connect(ctx, c.Config(spec.UID, "member"))
	require.NoError(t, err)
	require.NoError(t, s.Close())
}
