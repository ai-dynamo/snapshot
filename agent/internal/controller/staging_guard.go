// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
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
func (w *NodeController) checkStagingCapacity(ctx context.Context, containerPID int) error {
	stagingPath := w.config.PageBroker.StagingPath
	if stagingPath == "" {
		return nil
	}
	logger := logr.FromContextOrDiscard(ctx)
	total, err := w.stagingCapacityFn(stagingPath)
	if err != nil {
		logger.V(1).Info("Cannot measure staging; skipping the capacity check", "error", err.Error())
		return nil
	}
	pids := snapshotruntime.ProcessTreePIDs(containerPID)
	minimum, err := w.minimumImageSizeFn(snapshotruntime.HostProcPath, pids)
	if err != nil {
		logger.V(1).Info("Cannot size the image; skipping the capacity check", "error", err.Error())
		return nil
	}
	if shortfall := snapshotruntime.StagingShortfall(minimum, total); shortfall > 0 {
		return fmt.Errorf("image needs at least %d bytes of staging but the volume holds %d, short by %d",
			minimum, total, shortfall)
	}
	return nil
}

// refineFullStaging tells a staging volume that is momentarily occupied from one
// this image can never fit, for a dump that ran out of room part-written.
//
// The preflight check only ever establishes "too big", never "fits", so a
// capture it let through can still hit ENOSPC. Which condition that was cannot
// be read off the error — only measuring says, and the answer decides whether
// retrying is worth anything.
func (w *NodeController) refineFullStaging(ctx context.Context, failure captureFailure, err error, containerPID int) (captureFailure, error) {
	// PageBroker chooses where a dump writes, so the volume that filled is the
	// one the dump names — not necessarily the node's staging volume.
	var spaceErr *executor.DumpSpaceError
	if !errors.As(err, &spaceErr) || spaceErr.Dir == "" {
		return failure, err
	}
	logger := logr.FromContextOrDiscard(ctx)
	// PageBroker releases a failed transaction's directory before the failure is
	// classified, so the path the dump named is usually already gone. Its nearest
	// surviving ancestor is on the same filesystem and reports the same capacity.
	total, capacityErr := w.stagingCapacityFn(nearestExistingDir(spaceErr.Dir))
	if capacityErr != nil {
		logger.V(1).Info("Cannot measure staging; reporting the dump failure unrefined", "error", capacityErr.Error())
		return failure, err
	}
	minimum, sizeErr := w.minimumImageSizeFn(snapshotruntime.HostProcPath, snapshotruntime.ProcessTreePIDs(containerPID))
	if sizeErr != nil {
		logger.V(1).Info("Cannot size the image; reporting the dump failure unrefined", "error", sizeErr.Error())
		return failure, err
	}
	if shortfall := snapshotruntime.StagingShortfall(minimum, total); shortfall > 0 {
		return captureFailure{reason: reasonImageTooLarge}, fmt.Errorf(
			"%w: image needs at least %d bytes but %s holds %d, short by %d",
			err, minimum, spaceErr.Dir, total, shortfall)
	}
	// It fits, so the volume was occupied rather than too small — which frees up.
	return captureFailure{reason: reasonStagingUnavailable, retryable: true}, err
}

// nearestExistingDir walks up to the first directory that is still there.
func nearestExistingDir(dir string) string {
	for current := dir; ; {
		if _, err := os.Stat(current); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return current
		}
		current = parent
	}
}
