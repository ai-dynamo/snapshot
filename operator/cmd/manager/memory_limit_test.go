// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"math"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetMemoryLimit(t *testing.T) {
	// A limit no case below produces, so an untouched runtime is recognizable.
	const untouched = int64(1) << 40
	original := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(original) })

	for _, tc := range []struct {
		name, value string
		want        int64
		invalid     bool
	}{
		{name: "no container limit", value: ""},
		{name: "zero", value: "0"},
		{name: "chart default of 256Mi", value: "268435456", want: 241591910},
		{name: "1G", value: "1000000000", want: 900000000},
		{name: "1Gi", value: "1073741824", want: 966367641},
		{name: "rounds down", value: "19", want: 17},
		{name: "largest limit", value: "9223372036854775807", want: math.MaxInt64/10*9 + 6},
		{name: "negative", value: "-1", invalid: true},
		{name: "a quantity, not bytes", value: "256Mi", invalid: true},
		{name: "a fraction", value: "268435456.5", invalid: true},
		{name: "more than an int64", value: "9223372036854775808", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			debug.SetMemoryLimit(untouched)

			got, err := setMemoryLimit(tc.value)

			if tc.invalid {
				require.ErrorContains(t, err, memoryLimitEnv)
				require.ErrorContains(t, err, tc.value)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, got)
			runtimeLimit := untouched
			if tc.want > 0 {
				runtimeLimit = tc.want
			}
			require.Equal(t, runtimeLimit, debug.SetMemoryLimit(-1))
		})
	}
}
