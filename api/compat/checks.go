// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
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

// CheckImageDigest refuses a restore into image content other than what was
// captured. The reference is not compared: the same content is reachable under
// more than one, and a rebuilt or moved tag is one reference over two contents.
const CheckImageDigest Check = "image-digest"

var imageDigestCheck = check{
	name: CheckImageDigest,
	gate: GatePreflight,
	compare: func(source, target Facts) []Mismatch {
		return mustMatch(imageDigest(source.ImageID), imageDigest(target.ImageID))
	},
}

// CheckMemoryLimit refuses a restore into less memory than the checkpoint was
// captured with. Restoring faults the whole recorded address space back in, so a
// lower ceiling is not a slower restore but an OOM kill partway through one.
const CheckMemoryLimit Check = "memory-limit"

var memoryLimitCheck = check{
	name: CheckMemoryLimit,
	gate: GatePreflight,
	compare: func(source, target Facts) []Mismatch {
		return atLeastSource(source.MemoryLimit, target.MemoryLimit)
	},
}

// CheckCPULimit refuses a restore into less CPU than the checkpoint was captured
// with. Unlike memory, too little does not fail: the workload restores, reports
// success, and runs measurably slower forever, which is the worst outcome of the
// three because nothing says it happened.
const CheckCPULimit Check = "cpu-limit"

var cpuLimitCheck = check{
	name: CheckCPULimit,
	gate: GatePreflight,
	compare: func(source, target Facts) []Mismatch {
		return atLeastSource(source.CPULimit, target.CPULimit)
	},
}

// atLeastSource reports a mismatch when the target is given less than the
// checkpoint was captured with. A quantity absent or unreadable on either side is
// unknown - which is also how an unlimited pod reads, since a pod with no limit
// records none.
//
// It is deliberately blunt: a deployment that was genuinely over-provisioned and
// is being trimmed on purpose is refused too, and the escape hatch is the way
// out of that.
func atLeastSource(source, target string) []Mismatch {
	sourceQuantity, err := resource.ParseQuantity(source)
	if err != nil {
		return nil
	}
	targetQuantity, err := resource.ParseQuantity(target)
	if err != nil {
		return nil
	}
	if targetQuantity.Cmp(sourceQuantity) >= 0 {
		return nil
	}
	return []Mismatch{{Source: source, Target: target}}
}

// imageDigest reduces a container status image ID to the digest inside it.
// Runtimes disagree on the wrapping - containerd reports a bare "sha256:...",
// others a scheme and a repository around it - and the artifact keeps whichever
// form it was given, so the two are only comparable after this.
func imageDigest(imageID string) string {
	digest := strings.TrimSpace(imageID)
	if scheme := strings.Index(digest, "://"); scheme >= 0 {
		digest = digest[scheme+len("://"):]
	}
	if at := strings.LastIndex(digest, "@"); at >= 0 {
		digest = digest[at+1:]
	}
	return digest
}

// CheckMount refuses a restore into a pod that is missing a path the checkpoint
// had mounted. CRIU was told to leave those mounts alone and expect them to be
// there; where one is absent, the restored process gets a working directory or a
// dataset that simply is not there, and finds out by failing later.
const CheckMount Check = "mount"

// criuHandledMounts are recorded as externalized but reconstructed by CRIU
// itself, so their absence from the target pod is not a missing volume.
var criuHandledMounts = map[string]bool{
	"/":        true,
	"/dev/shm": true,
}

var mountCheck = check{
	name: CheckMount,
	gate: GateInspect,
	compare: func(source, target Facts) []Mismatch {
		existing := make(map[string]bool, len(target.ExistingMounts))
		for _, path := range target.ExistingMounts {
			existing[path] = true
		}

		var mismatches []Mismatch
		for _, path := range source.ExternalizedMounts {
			if existing[path] || criuHandledMounts[path] {
				continue
			}
			mismatches = append(mismatches, Mismatch{Source: path, Target: "missing"})
		}
		return mismatches
	},
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
