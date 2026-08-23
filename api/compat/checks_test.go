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

func TestKernelVersionCheck(t *testing.T) {
	kernel := func(value string) Facts {
		return Facts{KernelVersion: value}
	}

	tests := []struct {
		name   string
		source Facts
		target Facts
		want   []Mismatch
	}{
		{
			name:   "same kernel release",
			source: kernel("5.15.0-1071-aws"),
			target: kernel("5.15.0-1071-aws"),
		},
		{
			// A kernel upgrade alone is enough to break a restore that worked,
			// so the same major.minor on a different release is not good enough.
			name:   "same major and minor, different release",
			source: kernel("5.15.0-1071-aws"),
			target: kernel("5.15.0-1082-aws"),
			want: []Mismatch{{
				Check:  CheckKernelVersion,
				Source: "5.15.0-1071-aws",
				Target: "5.15.0-1082-aws",
			}},
		},
		{
			name:   "checkpoint taken before the kernel was recorded",
			target: kernel("5.15.0-1071-aws"),
		},
		{
			name:   "target kernel unknown",
			source: kernel("5.15.0-1071-aws"),
		},
		{
			// Both rules fire: the node runs a different kernel, and one no
			// restore of a modern glibc can succeed on.
			name:   "target below the floor",
			source: kernel("5.15.0-1071-aws"),
			target: kernel("5.4.0-150-generic"),
			want: []Mismatch{
				{Check: CheckKernelVersion, Source: "5.15.0-1071-aws", Target: "5.4.0-150-generic"},
				{Check: CheckKernelMinimum, Source: "5.13 or newer", Target: "5.4.0-150-generic"},
			},
		},
		{
			name:   "both sides below the floor",
			source: kernel("4.19.0-25-amd64"),
			target: kernel("4.19.0-25-amd64"),
			want: []Mismatch{
				{Check: CheckKernelMinimum, Source: "5.13 or newer", Target: "4.19.0-25-amd64"},
			},
		},
		{
			name:   "exactly at the floor",
			source: kernel("5.13.0-52-generic"),
			target: kernel("5.13.0-52-generic"),
		},
		{
			name:   "a newer major is above the floor",
			source: kernel("6.8.0-45-generic"),
			target: kernel("6.8.0-45-generic"),
		},
		{
			// A release the floor cannot read is unknown rather than old, so a
			// kernel string in a form nobody anticipated does not refuse every
			// restore on the node.
			name:   "unreadable release",
			source: kernel("custom-build"),
			target: kernel("custom-build"),
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
}

func TestImageDigestCheck(t *testing.T) {
	const (
		captured = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		rebuilt  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)
	imageID := func(id string) Facts {
		return Facts{ImageID: id}
	}

	tests := []struct {
		name   string
		source Facts
		target Facts
		want   []Mismatch
	}{
		{
			name:   "same content",
			source: imageID(captured),
			target: imageID(captured),
		},
		{
			// The same reference resolved to different content, which is what a
			// rebuilt or moved tag looks like from here.
			name:   "same reference, rebuilt content",
			source: imageID(captured),
			target: imageID(rebuilt),
			want:   []Mismatch{{Check: CheckImageDigest, Source: captured, Target: rebuilt}},
		},
		{
			// Runtimes wrap the digest differently, and the artifact keeps
			// whatever it was given. The same content must not read as a
			// mismatch because one side spells it out and the other does not.
			name:   "the same digest wrapped differently",
			source: imageID("docker-pullable://nvcr.io/nvidia/tritonserver@" + captured),
			target: imageID(captured),
		},
		{
			name:   "different content, wrapped differently",
			source: imageID("docker-pullable://nvcr.io/nvidia/tritonserver@" + captured),
			target: imageID(rebuilt),
			want:   []Mismatch{{Check: CheckImageDigest, Source: captured, Target: rebuilt}},
		},
		{
			name:   "checkpoint taken before the image ID was recorded",
			target: imageID(captured),
		},
		{
			// The kubelet has not published a status for the placeholder yet.
			name:   "target image ID not published yet",
			source: imageID(captured),
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
}

func TestMemoryLimitCheck(t *testing.T) {
	memory := func(limit string) Facts {
		return Facts{MemoryLimit: limit}
	}

	tests := []struct {
		name   string
		source Facts
		target Facts
		want   []Mismatch
	}{
		{
			name:   "the same limit",
			source: memory("32Gi"),
			target: memory("32Gi"),
		},
		{
			name:   "a larger limit",
			source: memory("32Gi"),
			target: memory("64Gi"),
		},
		{
			name:   "a smaller limit",
			source: memory("32Gi"),
			target: memory("1Gi"),
			want:   []Mismatch{{Check: CheckMemoryLimit, Source: "32Gi", Target: "1Gi"}},
		},
		{
			// The same amount written another way is the same amount.
			name:   "the same limit in different units",
			source: memory("32Gi"),
			target: memory("34359738368"),
		},
		{
			// A pod with no limit records none, so this is also how a restore
			// into an unlimited pod reads: nothing to compare.
			name:   "the target has no limit",
			source: memory("32Gi"),
		},
		{
			name:   "the checkpoint recorded no limit",
			target: memory("1Gi"),
		},
		{
			name:   "an unreadable quantity",
			source: memory("32Gi"),
			target: memory("plenty"),
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
}

func TestCPULimitCheck(t *testing.T) {
	cpu := func(limit string) Facts {
		return Facts{CPULimit: limit}
	}

	tests := []struct {
		name   string
		source Facts
		target Facts
		want   []Mismatch
	}{
		{
			name:   "the same limit",
			source: cpu("4"),
			target: cpu("4"),
		},
		{
			name:   "a larger limit",
			source: cpu("4"),
			target: cpu("8"),
		},
		{
			name:   "a smaller limit",
			source: cpu("4"),
			target: cpu("1"),
			want:   []Mismatch{{Check: CheckCPULimit, Source: "4", Target: "1"}},
		},
		{
			// Millicores and whole cores are the same scale, so the comparison
			// has to see through the notation.
			name:   "millicores below whole cores",
			source: cpu("4"),
			target: cpu("500m"),
			want:   []Mismatch{{Check: CheckCPULimit, Source: "4", Target: "500m"}},
		},
		{
			name:   "millicores above whole cores",
			source: cpu("1"),
			target: cpu("4500m"),
		},
		{
			name:   "the same limit written as millicores",
			source: cpu("4"),
			target: cpu("4000m"),
		},
		{
			name:   "the target has no limit",
			source: cpu("4"),
		},
		{
			name:   "the checkpoint recorded no limit",
			target: cpu("1"),
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
}

func TestMountCheck(t *testing.T) {
	mounts := func(externalized, existing []string) Facts {
		return Facts{ExternalizedMounts: externalized, ExistingMounts: existing}
	}

	tests := []struct {
		name  string
		facts Facts
		want  []Mismatch
	}{
		{
			name:  "every mount is there",
			facts: mounts([]string{"/model-cache", "/data"}, []string{"/model-cache", "/data"}),
		},
		{
			name:  "one mount is missing",
			facts: mounts([]string{"/model-cache", "/data"}, []string{"/model-cache"}),
			want:  []Mismatch{{Check: CheckMount, Source: "/data", Target: "missing"}},
		},
		{
			// Each missing volume is named, since a user fixing their pod needs
			// to know about all of them and not one at a time.
			name:  "the pod has none of them",
			facts: mounts([]string{"/model-cache", "/data"}, nil),
			want: []Mismatch{
				{Check: CheckMount, Source: "/model-cache", Target: "missing"},
				{Check: CheckMount, Source: "/data", Target: "missing"},
			},
		},
		{
			// CRIU reconstructs these itself, so their absence from the pod is
			// not a volume anybody forgot to declare.
			name:  "the mounts CRIU restores itself",
			facts: mounts([]string{"/", "/dev/shm", "/model-cache"}, []string{"/model-cache"}),
		},
		{
			name:  "the checkpoint externalized nothing",
			facts: mounts(nil, nil),
		},
		{
			// The target side is resolved from the recorded list, so a path the
			// pod has and the checkpoint never used is not the gate's business.
			name:  "the pod has more than the checkpoint used",
			facts: mounts([]string{"/model-cache"}, []string{"/model-cache", "/scratch"}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare(GateInspect, tc.facts, tc.facts)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Compare = %+v, want %+v", got, tc.want)
			}
		})
	}

	// Whether a path resolves is only knowable once the placeholder container
	// exists, which is after the first gate has already run.
	missing := mounts([]string{"/model-cache"}, nil)
	if got := Compare(GatePreflight, missing, missing); len(got) != 0 {
		t.Errorf("the first gate judged a mount it cannot see: %+v", got)
	}
}
