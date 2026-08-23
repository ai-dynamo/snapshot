// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"strconv"
	"strings"
)

// CheckCPUArch refuses a restore onto a different instruction set. A checkpoint
// holds register state, and CRIU has nowhere to put an x86 register file on an
// ARM core, so this one can never be waived by a bigger machine or a newer
// driver - it is the hardest of the rules.
const CheckCPUArch Check = "cpu-arch"

var cpuArchCheck = check{
	name:    CheckCPUArch,
	gate:    GatePreflight,
	compare: func(source, target Facts) []Mismatch { return mustMatch(source.CPUArch, target.CPUArch) },
}

// CheckKernelVersion refuses a restore onto a kernel other than the captured
// one. Restores that had worked for over a year have broken on a kernel upgrade
// alone: criu#2636.
const CheckKernelVersion Check = "kernel-version"

// CheckKernelMinimum refuses a kernel too old to restore a modern glibc at all.
// glibc uses rseq, which needs 5.13 (criu#2229), and glibc 2.35 and newer
// segfault on restore below it (criu#2552).
const CheckKernelMinimum Check = "kernel-minimum"

const (
	minKernelMajor = 5
	minKernelMinor = 13
)

var kernelVersionCheck = check{
	name: CheckKernelVersion,
	gate: GatePreflight,
	compare: func(source, target Facts) []Mismatch {
		return mustMatch(source.KernelVersion, target.KernelVersion)
	},
}

var kernelMinimumCheck = check{
	name: CheckKernelMinimum,
	gate: GatePreflight,
	compare: func(_, target Facts) []Mismatch {
		major, minor, ok := parseKernelVersion(target.KernelVersion)
		if !ok || major > minKernelMajor || (major == minKernelMajor && minor >= minKernelMinor) {
			return nil
		}
		return []Mismatch{{
			Source: strconv.Itoa(minKernelMajor) + "." + strconv.Itoa(minKernelMinor) + " or newer",
			Target: target.KernelVersion,
		}}
	},
}

// parseKernelVersion reads the leading major.minor of a kernel release, which
// carries a distro suffix after it as in "5.15.0-1071-aws". A release it cannot
// read is unknown, which leaves the floor to the equality rule above.
func parseKernelVersion(version string) (major, minor int, ok bool) {
	fields := strings.SplitN(strings.TrimSpace(version), ".", 3)
	if len(fields) < 2 {
		return 0, 0, false
	}
	major, majorOK := leadingNumber(fields[0])
	minor, minorOK := leadingNumber(fields[1])
	return major, minor, majorOK && minorOK
}

// leadingNumber reads the digits a version field starts with, ignoring whatever
// a distro appended to them.
func leadingNumber(field string) (int, bool) {
	end := 0
	for end < len(field) && field[end] >= '0' && field[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	value, err := strconv.Atoi(field[:end])
	return value, err == nil
}

// mustMatch reports a mismatch unless the two values are identical. A value
// absent on either side is unknown, and an unknown fact never refuses a restore:
// a checkpoint captured before it was ever recorded has to stay restorable.
func mustMatch(source, target string) []Mismatch {
	if source == "" || target == "" || source == target {
		return nil
	}
	return []Mismatch{{Source: source, Target: target}}
}
