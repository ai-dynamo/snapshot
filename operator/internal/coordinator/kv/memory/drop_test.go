// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A session that has ended keeps its space until it is removed, so Drop must
// release what the space holds, not only forget it.
func TestDropEmptiesTheSpaceForWhoeverStillHoldsIt(t *testing.T) {
	ctx := context.Background()
	st := New()
	held := st.Space("attempt").(*space)
	require.NoError(t, held.Insert(ctx, "key", make([]byte, 1<<20)))
	require.NoError(t, held.CreateBarrier(ctx, "barrier", 2))
	require.NoError(t, held.PostBarrier(ctx, "barrier", "participant"))

	require.NoError(t, st.Drop(ctx, "attempt"))

	assert.Empty(t, held.values)
	assert.Empty(t, held.waiters)
	assert.Empty(t, held.barriers)
	assert.NotSame(t, held, st.Space("attempt"), "the attempt's next space starts afresh")
}
