// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

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

// mustMatch reports a mismatch unless the two values are identical. A value
// absent on either side is unknown, and an unknown fact never refuses a restore:
// a checkpoint captured before it was ever recorded has to stay restorable.
func mustMatch(source, target string) []Mismatch {
	if source == "" || target == "" || source == target {
		return nil
	}
	return []Mismatch{{Source: source, Target: target}}
}
