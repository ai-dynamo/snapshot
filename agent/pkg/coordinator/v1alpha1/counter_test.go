// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatorv1alpha1

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACounterRoundTrips(t *testing.T) {
	for _, n := range []int64{0, 1, -1, math.MaxInt64, math.MinInt64} {
		got, err := DecodeCounter(EncodeCounter(n))

		require.NoError(t, err)
		assert.Equal(t, n, got)
	}
}

func TestACounterIsBigEndianTwosComplement(t *testing.T) {
	assert.Equal(t, []byte{0, 0, 0, 0, 0, 0, 0x01, 0x02}, EncodeCounter(0x0102))
	assert.Equal(t, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe}, EncodeCounter(-2))
}

func TestAValueOfAnyOtherLengthIsNotACounter(t *testing.T) {
	for _, value := range [][]byte{nil, make([]byte, CounterSize-1), make([]byte, CounterSize+1)} {
		_, err := DecodeCounter(value)

		assert.Error(t, err, "%d bytes", len(value))
	}
}
