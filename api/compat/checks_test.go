// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"reflect"
	"testing"
)

func TestCPUArchCheck(t *testing.T) {
	arch := func(value string) Facts {
		return Facts{CPUArch: value}
	}

	tests := []struct {
		name   string
		source Facts
		target Facts
		want   []Mismatch
	}{
		{
			name:   "same architecture",
			source: arch("amd64"),
			target: arch("amd64"),
		},
		{
			name:   "different architecture",
			source: arch("amd64"),
			target: arch("arm64"),
			want:   []Mismatch{{Check: CheckCPUArch, Source: "amd64", Target: "arm64"}},
		},
		{
			name:   "checkpoint taken before the architecture was recorded",
			target: arch("arm64"),
		},
		{
			name:   "target architecture unknown",
			source: arch("amd64"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare(GatePreflight, tc.source, tc.target)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Compare = %+v, want %+v", got, tc.want)
			}
		})
	}

	// The architecture is decidable from the manifest and the node alone, so
	// waiting for the placeholder container would delay the refusal for nothing.
	if got := Compare(GateInspect, arch("amd64"), arch("arm64")); len(got) != 0 {
		t.Errorf("the second gate repeated the architecture check: %+v", got)
	}
}
