// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInterruptSparesALaterConnection checks that a disconnect makes the
// operations sent over the connection that broke, and over any before it, send
// their requests again, and leaves alone those sent over a later connection,
// which nats.go may have made by the time it reports the disconnect.
func TestInterruptSparesALaterConnection(t *testing.T) {
	var f inflight
	ops := make([]context.Context, 3)
	for connection := range ops {
		ctx, cancel := context.WithCancelCause(context.Background())
		t.Cleanup(func() { cancel(nil) })
		f.add(uint64(connection), cancel)
		ops[connection] = ctx
	}
	f.interrupt(2)
	for connection, ctx := range ops[:2] {
		require.ErrorIs(t, context.Cause(ctx), errRetry, "an operation of connection %d", connection)
	}
	assert.NoError(t, ops[2].Err(), "an operation of the later connection must be left alone")

	// A disconnect reported while no connection is up interrupts every one.
	f.interrupt(math.MaxUint64)
	require.ErrorIs(t, context.Cause(ops[2]), errRetry)
}
