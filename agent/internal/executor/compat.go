// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// refuseMIGCapture fails a checkpoint of a container on a MIG slice. It runs
// before anything is locked, so the workload keeps serving.
func refuseMIGCapture(ctx context.Context, log logr.Logger, pid int) error {
	migSlices := visibleMIGSlices(ctx, log, pid)
	if len(migSlices) == 0 {
		return nil
	}
	return fmt.Errorf("refused as incompatible: %s: the container holds MIG slice %s, and Snapshot supports whole GPUs only",
		compat.CheckMIGSlice, strings.Join(migSlices, ", "))
}

// refuseMIGRestore is the inspect gate's MIG rule. It cannot wait for
// inspectCompatibility, which takes the target GPUs that discovery would first
// fail to read or misreport on a slice.
func refuseMIGRestore(ctx context.Context, log logr.Logger, pid int, skipCompatCheck bool) error {
	if skipCompatCheck {
		return nil
	}
	migSlices := visibleMIGSlices(ctx, log, pid)
	if len(migSlices) == 0 {
		return nil
	}
	return compat.NewIncompatibleError(compat.GateInspect, []compat.Mismatch{{
		Check:  compat.CheckMIGSlice,
		Source: "whole GPU",
		Target: "MIG slice " + strings.Join(migSlices, ", "),
	}})
}

// visibleMIGSlices treats a container whose slices cannot be listed as holding
// whole GPUs. A real slice still fails the capture or restore, only later and
// less legibly.
func visibleMIGSlices(ctx context.Context, log logr.Logger, pid int) []string {
	migSlices, err := cuda.DiscoverMIGSlices(ctx, snapshotruntime.HostProcPath, pid)
	if err != nil {
		log.V(1).Info("Failed to list MIG slices; treating the GPUs as whole", "pid", pid, "error", err)
		return nil
	}
	return migSlices
}

// inspectCompatibility runs the inspect gate for one restore, the counterpart of
// the controller's preflightCompatibility. A nil error means the restore may go
// ahead. It gathers the target state this gate can read, which the earlier gate
// cannot: the runtime image ID, GPUs, and mounts under its rootfs.
func inspectCompatibility(
	log logr.Logger,
	manifest *types.CheckpointManifest,
	targetGPUs compat.GPUInfo,
	gpuMountAliases map[string]string,
	targetRoot string,
	targetImageID string,
	skipCompatCheck bool,
) error {
	if skipCompatCheck {
		log.Info("Restore compatibility check skipped by request", "gate", string(compat.GateInspect))
		return nil
	}

	sourceEnv := manifest.CompatEnvironment()
	if manifest.CuInterpose != nil {
		// Restore installs and validates this Snapshot-owned mount after
		// inspection. Workload mounts must already exist in the placeholder.
		sourceEnv.ExternalizedMounts = slices.DeleteFunc(sourceEnv.ExternalizedMounts, func(path string) bool {
			return path == podcontract.CuInterposeMountPath
		})
	}
	targetEnv := compat.Environment{
		ImageID:            targetImageID,
		DriverVersion:      targetGPUs.DriverVersion,
		GPUDevices:         targetGPUs.Devices,
		ExistingMountPaths: existingMountPaths(targetRoot, sourceEnv.ExternalizedMounts, gpuMountAliases),
	}
	mismatches := compat.Compare(compat.GateInspect, sourceEnv, targetEnv)
	if len(mismatches) == 0 {
		return nil
	}
	return compat.NewIncompatibleError(compat.GateInspect, mismatches)
}
