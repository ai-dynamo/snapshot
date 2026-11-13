// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// refineFullStaging separates the storage conditions a part-written dump can hit.
//
// The preflight only ever establishes "too big", never "fits", so only measuring
// says whether retrying this one is worth anything. Which volume filled matters
// as much as how full it is: staging is transient and frees up between attempts,
// while the artifact store holds committed checkpoints and does not.
func (w *NodeController) refineFullStaging(ctx context.Context, failure captureFailure, err error, containerPID int) (captureFailure, error) {
	var spaceErr *executor.DumpSpaceError
	if !errors.As(err, &spaceErr) || spaceErr.Dir == "" {
		return failure, err
	}
	logger := logr.FromContextOrDiscard(ctx)
	// A dump that reached the store rather than staging filled a volume no retry
	// on this node frees: the room there is taken by committed checkpoints.
	if !w.isStagingDir(spaceErr.Dir) {
		return captureFailure{reason: reasonStoreExhausted}, err
	}
	// The dump's directory is usually released before the failure is classified;
	// its nearest surviving ancestor reports the same capacity.
	measured, ok := stagingAncestor(spaceErr.Dir, w.config.PageBroker.StagingPath)
	if !ok {
		logger.V(1).Info("The dump's volume is gone; reporting the failure unrefined", "dir", spaceErr.Dir)
		return failure, err
	}
	total, capacityErr := w.stagingCapacityFn(measured)
	if capacityErr != nil {
		logger.V(1).Info("Cannot measure staging; reporting the dump failure unrefined", "error", capacityErr.Error())
		return failure, err
	}
	pids := snapshotruntime.ProcessTreePIDs(containerPID)
	if len(pids) == 0 {
		// The dump already terminated the tree, so there is nothing left to size
		// and a zero would read as "it fits".
		logger.V(1).Info("The source is gone; reporting the dump failure unrefined")
		return failure, err
	}
	minimum, sizeErr := w.minimumImageSizeFn(snapshotruntime.HostProcPath, pids)
	if sizeErr != nil {
		logger.V(1).Info("Cannot size the image; reporting the dump failure unrefined", "error", sizeErr.Error())
		return failure, err
	}
	if shortfall := snapshotruntime.StagingShortfall(minimum, total); shortfall > 0 {
		return captureFailure{reason: reasonImageTooLarge}, fmt.Errorf(
			"%w: image needs at least %d bytes but %s holds %d, short by %d",
			err, minimum, spaceErr.Dir, total, shortfall)
	}
	// minimum is a floor, so this says the image was not provably too big — not
	// that it fits. Staging being merely occupied is the condition a retry can
	// clear, and the numbers go in the log so the guess is auditable.
	logger.V(1).Info("The image did not provably exceed its volume; treating the shortage as contention",
		"minimum_bytes", minimum, "volume_bytes", total, "dir", spaceErr.Dir)
	return captureFailure{reason: reasonStagingUnavailable, retryable: true}, err
}

// isStagingDir reports whether a dump wrote to the node's staging volume. An
// unset staging path means the agent has none, so nothing it dumped was staged.
func (w *NodeController) isStagingDir(dir string) bool {
	staging := w.config.PageBroker.StagingPath
	if staging == "" {
		return false
	}
	rel, err := filepath.Rel(staging, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// stagingAncestor walks up to the first directory that is still there, stopping
// at the staging root. Going past it would measure an unrelated filesystem —
// eventually the agent's own rootfs — and report a capacity that was never the
// dump's.
func stagingAncestor(dir, staging string) (string, bool) {
	// filepath.Dir cleans as it walks, so an uncleaned root would never compare
	// equal and the walk would pass the very boundary it is here to stop at.
	staging = filepath.Clean(staging)
	for current := filepath.Clean(dir); ; {
		if _, err := os.Stat(current); err == nil {
			return current, true
		}
		if current == staging {
			return "", false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false
		}
		current = parent
	}
}

// dumpWorthRepeating refuses to repeat a dump nothing could make fit.
//
// The probe answers whether another dump can run, not whether it could end
// differently; an image too large for its volume ends the same way every time.
func (w *NodeController) dumpWorthRepeating(ctx context.Context, containerPID int) func(error) (bool, string) {
	return func(err error) (bool, string) {
		failure, _ := w.refineFullStaging(ctx, classifyCaptureFailure(err), err, containerPID)
		switch failure.reason {
		case reasonImageTooLarge:
			return false, "the image cannot fit the volume it is written to"
		case reasonStoreExhausted:
			return false, "the store it is written to is full, and no repeat here frees it"
		}
		return true, ""
	}
}
