// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"math"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemoryLimit(t *testing.T) {
	original := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(original) })
	for _, tc := range []struct {
		name, value string
		want        int64
		invalid     bool
	}{
		{name: "no limit", want: 123456},
		{name: "zero", value: "0", want: 123456},
		{name: "container limit", value: "1073741824", want: 966367641},
		{name: "largest limit", value: "9223372036854775807", want: math.MaxInt64/10*9 + 6},
		{name: "negative", value: "-1", want: 123456, invalid: true},
		{name: "not bytes", value: "1Gi", want: 123456, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			debug.SetMemoryLimit(123456)
			t.Setenv(MemoryLimitEnv, tc.value)
			err := setMemoryLimit()
			if tc.invalid {
				require.ErrorContains(t, err, MemoryLimitEnv)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, debug.SetMemoryLimit(-1))
		})
	}
}
