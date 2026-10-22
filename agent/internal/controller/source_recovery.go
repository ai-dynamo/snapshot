// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

// recoverSourceAfterFailedDump decides what becomes of a workload whose dump
// failed: hand it back its GPUs and leave it running, or kill it.
//
// The dump does not reach this path on success — it terminates the source
// itself. On failure the workload is suspended but intact, so killing it
// immediately destroys a workload that could have carried on, and destroys the
// state any retry would build on. It is killed only when it cannot be put back:
// the GPU state is unrecoverable, or CRIU left a task seized or stopped, in
// which case it would hang rather than resume.
func (w *NodeController) recoverSourceAfterFailedDump(ctx context.Context, log logr.Logger, containerPID int) {
	if reason, ok := w.reviveSource(ctx, log, containerPID); !ok {
		log.Info("Killing the source after a failed dump", "reason", reason)
		if err := w.killCheckpointProcess(log, containerPID, "checkpoint failed"); err != nil {
			log.Error(err, "Failed to kill target after checkpoint failure")
		}
		return
	}
	log.Info("Source left running after a failed dump", "pid", containerPID)
}

// reviveSource reports whether the workload is running again, and why not when
// it is not.
func (w *NodeController) reviveSource(ctx context.Context, log logr.Logger, containerPID int) (string, bool) {
	pids := snapshotruntime.ProcessTreePIDs(containerPID)
	if len(pids) == 0 {
		return "the process tree is gone", false
	}

	// The GPUs come back first: a checkpointed process cannot run without them.
	gpuPIDs := w.filterCUDAProcessesFn(ctx, pids, log)
	if len(gpuPIDs) > 0 {
		if err := w.reviveGPUProcessesFn(ctx, gpuPIDs, cuda.DefaultHelperBinaryPath, log); err != nil {
			log.Error(err, "Could not hand the GPUs back to the source")
			return "the GPU state could not be recovered", false
		}
	}

	// Then CRIU: a task still seized or stopped cannot resume on its own.
	if released, reason := snapshotruntime.Released(snapshotruntime.HostProcPath, pids); !released {
		return reason, false
	}
	return "", true
}
