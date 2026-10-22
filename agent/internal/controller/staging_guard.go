// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

// reasonImageTooLarge ends a capture the node cannot stage, as distinct from
// staging being momentarily full, which another attempt may find free.
const reasonImageTooLarge = "ImageTooLarge"

// checkStagingCapacity refuses a capture whose image provably cannot fit in the
// node's staging volume.
//
// Writing it anyway fills a memory-backed volume shared with everything else on
// the node. The estimate is a lower bound, so this establishes "too big" and
// never "fits": a capture that passes here can still run out of room, which
// arrives later as ENOSPC.
func (w *NodeController) checkStagingCapacity(containerPID int) error {
	stagingPath := w.config.PageBroker.StagingPath
	if stagingPath == "" {
		return nil
	}
	available, err := snapshotruntime.StagingCapacity(stagingPath)
	if err != nil {
		w.log.V(1).Info("Cannot measure staging; skipping the capacity check", "error", err.Error())
		return nil
	}
	pids := snapshotruntime.ProcessTreePIDs(containerPID)
	estimate, err := snapshotruntime.EstimateImageSize(snapshotruntime.HostProcPath, pids)
	if err != nil {
		w.log.V(1).Info("Cannot estimate the image; skipping the capacity check", "error", err.Error())
		return nil
	}
	if shortfall := snapshotruntime.StagingShortfall(estimate, available); shortfall > 0 {
		return fmt.Errorf("image needs at least %d bytes of staging but only %d are available, short by %d",
			estimate, available, shortfall)
	}
	return nil
}
