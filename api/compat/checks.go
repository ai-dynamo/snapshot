// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"sort"
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
	gate: GateInspect,
	compare: func(source, target Facts) []Mismatch {
		return mustMatch(ImageDigest(source.ImageID), ImageDigest(target.ImageID))
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

// ImageDigest reduces a container status image ID to the digest inside it.
// Runtimes disagree on the wrapping - containerd reports a bare "sha256:...",
// others a scheme and a repository around it - and the artifact keeps whichever
// form it was given, so the two are only comparable after this.
//
// Exported so that what a checkpoint publishes is the same reduction this
// compares, rather than a second one that can disagree with it.
func ImageDigest(imageID string) string {
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
		existing := make(map[string]bool, len(target.ExistingMountPaths))
		for _, path := range target.ExistingMountPaths {
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

// CheckGPUModel refuses a restore onto a different GPU model. A CUDA checkpoint
// carries device state built for one architecture's memory layout and
// capabilities, and no amount of driver compatibility makes an A100 replay what
// an L4 was doing.
const CheckGPUModel Check = "gpu-model"

var gpuModelCheck = check{
	name: CheckGPUModel,
	gate: GateInspect,
	compare: func(source, target Facts) []Mismatch {
		sourceModels, sourceOK := gpuModels(source.GPUDevices)
		targetModels, targetOK := gpuModels(target.GPUDevices)
		if !sourceOK || !targetOK || sourceModels == targetModels {
			return nil
		}
		return []Mismatch{{Source: sourceModels, Target: targetModels}}
	},
}

// CheckGPUCount refuses a restore onto a different number of GPUs. A multi-GPU
// checkpoint holds one piece of device state per GPU with a rank each, and there
// is no meaning to be given to a rank that has nowhere to land - or to a GPU no
// rank was recorded for.
//
// A target with no GPUs at all is that same refusal and is reported as one. It
// reaches here only once discovery has run, so none found means none, and the
// alternative is the unnamed device-map error further in.
const CheckGPUCount Check = "gpu-count"

var gpuCountCheck = check{
	name: CheckGPUCount,
	gate: GateInspect,
	compare: func(source, target Facts) []Mismatch {
		sourceCount := len(source.GPUDevices)
		targetCount := len(target.GPUDevices)
		if sourceCount == 0 || sourceCount == targetCount {
			return nil
		}
		return []Mismatch{{
			Source: strconv.Itoa(sourceCount),
			Target: strconv.Itoa(targetCount),
		}}
	},
}

// CheckDriverVersion refuses a restore on a driver build other than the captured
// one. Build granularity is not caution for its own sake: upstream reproduces a
// restore failure between 560.35.03 and 560.35.05.
const CheckDriverVersion Check = "driver-version"

// CheckDriverMinimum refuses a driver older than CUDA checkpoint and restore is
// supported on at all.
const CheckDriverMinimum Check = "driver-minimum"

const minDriverMajor = 580

var driverVersionCheck = check{
	name: CheckDriverVersion,
	gate: GateInspect,
	compare: func(source, target Facts) []Mismatch {
		return mustMatch(source.DriverVersion, target.DriverVersion)
	},
}

var driverMinimumCheck = check{
	name: CheckDriverMinimum,
	gate: GateInspect,
	compare: func(_, target Facts) []Mismatch {
		major, ok := leadingNumber(target.DriverVersion)
		if !ok || major >= minDriverMajor {
			return nil
		}
		return []Mismatch{{
			Source: strconv.Itoa(minDriverMajor) + " or newer",
			Target: target.DriverVersion,
		}}
	},
}

// gpuModels builds a stable model summary: sorting ignores allocation order,
// while "xN" preserves how many GPUs have each name. ProductName comes from
// nvidia-smi --query-gpu=name, documented as the GPU's official product name:
// https://docs.nvidia.com/deploy/nvidia-smi/index.html#product-name
//
// It returns unknown if any GPU has no name because partial data cannot prove
// that the source and target models differ.
func gpuModels(devices []GPUDevice) (string, bool) {
	if len(devices) == 0 {
		return "", false
	}
	counts := make(map[string]int, len(devices))
	for _, device := range devices {
		model := strings.TrimSpace(device.ProductName)
		if model == "" {
			return "", false
		}
		counts[model]++
	}

	models := make([]string, 0, len(counts))
	for model := range counts {
		models = append(models, model)
	}
	sort.Strings(models)
	for i, model := range models {
		models[i] = model + " x" + strconv.Itoa(counts[model])
	}
	return strings.Join(models, ", "), true
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
