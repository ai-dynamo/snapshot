// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"math"
	"testing"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheWireCounterCodecAgreesWithKV(t *testing.T) {
	require.Equal(t, kv.CounterSize, wire.CounterSize)
	for _, n := range []int64{0, 1, -1, 1 << 40, math.MaxInt64, math.MinInt64} {
		assert.Equal(t, kv.EncodeCounter(n), wire.EncodeCounter(n), "encoding %d", n)
		got, err := wire.DecodeCounter(kv.EncodeCounter(n))
		require.NoError(t, err)
		assert.Equal(t, n, got, "decoding %d", n)
	}
}
