// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import "testing"

func TestOrUnknown(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "a recorded value is its own", value: "580.82.07", want: "580.82.07"},
		{name: "an empty value is unknown", value: "", want: "unknown"},
		{name: "a blank value is unknown", value: "   ", want: "unknown"},
		{name: "a tab is unknown", value: "\t", want: "unknown"},
		{name: "surrounding space is kept", value: " 5.15.0 ", want: " 5.15.0 "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := OrUnknown(tc.value); got != tc.want {
				t.Errorf("OrUnknown(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}
