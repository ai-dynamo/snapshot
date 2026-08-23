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
// cannot: the GPUs the container sees and the mounts under its rootfs.
//
// No target fact is readable yet, so nothing can be refused here until the
// first rule of this gate arrives.
func inspectCompatibility(log logr.Logger, manifest *types.CheckpointManifest, skipCompatCheck bool) error {
	if skipCompatCheck {
		log.Info("Restore compatibility check skipped by request", "gate", string(compat.GateInspect))
		return nil
	}

	mismatches := compat.Compare(compat.GateInspect, manifest.CompatFacts(), compat.Facts{})
	if len(mismatches) == 0 {
		return nil
	}
	return compat.NewIncompatibleError(compat.GateInspect, mismatches)
}
