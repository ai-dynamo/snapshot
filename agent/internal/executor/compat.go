// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
)

// inspectCompatibility runs the inspect gate for one restore, the counterpart of
// the controller's preflightCompatibility. A nil error means the restore may go
// ahead. It gathers the target facts this gate can read, which the earlier gate
// cannot: the runtime image ID, GPUs, and mounts under its rootfs.
func inspectCompatibility(
	log logr.Logger,
	manifest *types.CheckpointManifest,
	targetGPUs compat.GPUFacts,
	targetRoot string,
	targetImageID string,
	skipCompatCheck bool,
) error {
	if skipCompatCheck {
		log.Info("Restore compatibility check skipped by request", "gate", string(compat.GateInspect))
		return nil
	}

	sourceFacts := manifest.CompatFacts()
	targetFacts := compat.Facts{
		ImageID:            targetImageID,
		DriverVersion:      targetGPUs.DriverVersion,
		GPUDevices:         targetGPUs.Devices,
		ExistingMountPaths: existingMountPaths(targetRoot, sourceFacts.ExternalizedMounts),
	}
	mismatches := compat.Compare(compat.GateInspect, sourceFacts, targetFacts)
	if len(mismatches) == 0 {
		return nil
	}
	return compat.NewIncompatibleError(compat.GateInspect, mismatches)
}
