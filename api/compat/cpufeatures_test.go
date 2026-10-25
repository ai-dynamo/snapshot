// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"reflect"
	"testing"
)

func TestInstructionSetFeatures(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  []string
	}{
		{
			name:  "machine description is dropped",
			flags: []string{"fpu", "hypervisor", "constant_tsc", "avx512f", "md_clear"},
			want:  []string{"avx512f", "fpu"},
		},
		{
			name:  "sorted and deduplicated",
			flags: []string{"sse2", "avx", "sse2", "avx"},
			want:  []string{"avx", "sse2"},
		},
		{
			// /proc/cpuinfo spells SSE3 as pni.
			name:  "SSE3 under its cpuinfo name",
			flags: []string{"pni", "sse3"},
			want:  []string{"pni"},
		},
		{
			name:  "instructions CRIU's mask omits",
			flags: []string{"clwb", "avx512ifma", "lahf_lm", "fma", "hypervisor"},
			want:  []string{"avx512ifma", "clwb", "fma", "lahf_lm"},
		},
		{
			name:  "entries of CRIU's mask that name no instruction",
			flags: []string{"rep_good", "tme", "monitor", "rtm"},
			want:  []string{"rtm"},
		},
		{
			name:  "no instruction-set flag at all is unknown",
			flags: []string{"hypervisor", "constant_tsc"},
		},
		{
			name: "nothing recorded",
		},
		{
			name:  "an empty list is unknown",
			flags: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := InstructionSetFeatures(tc.flags); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("InstructionSetFeatures(%v) = %#v, want %#v", tc.flags, got, tc.want)
			}
		})
	}
}
