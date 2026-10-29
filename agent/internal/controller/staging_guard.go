// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

// reasonImageTooLarge ends a capture this node can never stage. Staging that is
// merely occupied is a different condition: it frees up, and the broker reports
// it as StagingUnavailable.
const reasonImageTooLarge = "ImageTooLarge"

// checkStagingCapacity refuses a capture whose image cannot fit the node's
// staging volume however empty it is.
//
// Writing it anyway fills a memory-backed volume shared with everything else on
// the node. Both inputs are chosen so the comparison can only ever be right:
// the floor on what the dump must write, against the volume's total size. A
// capture that passes here can still run out of room, which arrives later as
// ENOSPC.
func (w *NodeController) checkStagingCapacity(containerPID int) error {
	stagingPath := w.config.PageBroker.StagingPath
	if stagingPath == "" {
		return nil
	}
	total, err := w.stagingCapacityFn(stagingPath)
	if err != nil {
		w.log.V(1).Info("Cannot measure staging; skipping the capacity check", "error", err.Error())
		return nil
	}
	pids := snapshotruntime.ProcessTreePIDs(containerPID)
	minimum, err := w.minimumImageSizeFn(snapshotruntime.HostProcPath, pids)
	if err != nil {
		w.log.V(1).Info("Cannot size the image; skipping the capacity check", "error", err.Error())
		return nil
	}
	if shortfall := snapshotruntime.StagingShortfall(minimum, total); shortfall > 0 {
		return fmt.Errorf("image needs at least %d bytes of staging but the volume holds %d, short by %d",
			minimum, total, shortfall)
	}
	return nil
}
