// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import "testing"

// Asserted literally: every rule added later inherits this shape rather than
// inventing its own, and the three report surfaces compare against it.
func TestMismatchReason(t *testing.T) {
	tests := []struct {
		name     string
		mismatch Mismatch
		want     string
	}{
		{
			name:     "both values known",
			mismatch: Mismatch{Check: "memory-limit", Source: "32Gi", Target: "1Gi"},
			want:     "memory-limit: source 32Gi, target 1Gi",
		},
		{
			name:     "source unrecorded",
			mismatch: Mismatch{Check: CheckImageDigest, Target: "sha256:beef"},
			want:     "image-digest: source unknown, target sha256:beef",
		},
		{
			name:     "target unreadable",
			mismatch: Mismatch{Check: CheckDriverVersion, Source: "580.82.07"},
			want:     "driver-version: source 580.82.07, target unknown",
		},
		{
			name:     "blank values are unknown",
			mismatch: Mismatch{Check: "kernel-version", Source: "  ", Target: ""},
			want:     "kernel-version: source unknown, target unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mismatch.Reason(); got != tc.want {
				t.Fatalf("Reason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReasons(t *testing.T) {
	tests := []struct {
		name       string
		mismatches []Mismatch
		want       string
	}{
		{
			name: "no mismatches",
			want: "",
		},
		{
			name:       "one mismatch",
			mismatches: []Mismatch{{Check: "cpu-arch", Source: "amd64", Target: "arm64"}},
			want:       "cpu-arch: source amd64, target arm64",
		},
		{
			name: "report order is preserved",
			mismatches: []Mismatch{
				{Check: "cpu-arch", Source: "amd64", Target: "arm64"},
				{Check: "memory-limit", Source: "32Gi", Target: "1Gi"},
			},
			want: "cpu-arch: source amd64, target arm64; memory-limit: source 32Gi, target 1Gi",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Reasons(tc.mismatches); got != tc.want {
				t.Fatalf("Reasons() = %q, want %q", got, tc.want)
			}
		})
	}
}
