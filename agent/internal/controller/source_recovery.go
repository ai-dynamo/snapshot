// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

// sourceRecoveryTimeout bounds handing the workload back its GPUs. The capture
// has already failed, so this cannot wait on it indefinitely.
const sourceRecoveryTimeout = 2 * time.Minute

// Event reasons for what became of a source Pod whose checkpoint failed.
const (
	reasonSourcePodResumed           = "SourcePodResumed"
	reasonSourcePodTerminated        = "SourcePodTerminated"
	reasonSourcePodTerminationFailed = "SourcePodTerminationFailed"
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
func (w *NodeController) recoverSourceAfterFailedDump(
	ctx context.Context,
	log logr.Logger,
	pod *corev1.Pod,
	containerPID int,
	suspended executor.SuspendedSource,
	retryWillFollow bool,
) {
	// Detached from the capture: a dump that failed on its deadline leaves that
	// context dead, and probing the GPUs through it returns no processes rather
	// than an error — so the workload would be reported left running with its
	// GPUs never handed back.
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sourceRecoveryTimeout)
	defer cancel()

	// Reviving a workload nothing will capture again only leaves it waiting for a
	// checkpoint that is not coming, which is worse than ending it.
	reason := "no further attempt will be made"
	revived := false
	if retryWillFollow {
		reason, revived = w.reviveSource(recoveryCtx, log, containerPID, suspended)
	}
	if !revived {
		log.Info("Terminating the source Pod after a failed checkpoint", "reason", reason)
		if err := w.killCheckpointProcess(log, containerPID, "checkpoint failed"); err != nil {
			// Saying it was terminated would leave the workload suspended while
			// the only thing an owner can see says it is gone.
			log.Error(err, "Failed to kill target after checkpoint failure")
			w.emitRecoveryEvent(recoveryCtx, log, pod, reasonSourcePodTerminationFailed,
				fmt.Sprintf("Could not terminate the source Pod after a failed checkpoint, so it may still be suspended: %v", err))
			return
		}
		w.emitRecoveryEvent(recoveryCtx, log, pod, reasonSourcePodTerminated,
			fmt.Sprintf("Terminated the source Pod after a failed checkpoint: %s", reason))
		return
	}
	log.Info("Source Pod resumed after a failed checkpoint", "pid", containerPID)
	w.emitRecoveryEvent(recoveryCtx, log, pod, reasonSourcePodResumed,
		"Resumed the source Pod after a failed checkpoint; it is still running")
}

// emitRecoveryEvent tells the source Pod's owner what became of their workload.
// The PodSnapshot only says the checkpoint failed, which does not say whether
// the workload is still alive.
func (w *NodeController) emitRecoveryEvent(ctx context.Context, log logr.Logger, pod *corev1.Pod, reason, message string) {
	if pod == nil {
		return
	}
	eventType := corev1.EventTypeWarning
	if reason == reasonSourcePodResumed {
		eventType = corev1.EventTypeNormal
	}
	emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, eventType, reason, message)
}

// reviveSource hands the workload back its GPUs and checks that CRIU let go,
// reporting whether it is running again and, when it is not, what stopped it.
// It changes the workload's state; the caller decides what to do with the
// answer.
func (w *NodeController) reviveSource(ctx context.Context, log logr.Logger, containerPID int, suspended executor.SuspendedSource) (string, bool) {
	// Preparing a multi-GPU capture removes the tree's shared mappings, and
	// nothing puts them back. Leaving it running would leave it running broken.
	if suspended.CuInterposePrepared {
		return "its shared GPU mappings were removed and cannot be restored", false
	}
	if suspended.CustomStorageUsed {
		return "its GPU memory went to PageBroker, whose execution is already torn down", false
	}

	pids := snapshotruntime.ProcessTreePIDs(containerPID)
	if len(pids) == 0 {
		return "the process tree is gone", false
	}

	// The GPUs come back first: a checkpointed process cannot run without them.
	// These are the processes the capture itself found, not a fresh probe.
	if len(suspended.GPUPIDs) > 0 {
		if err := w.reviveGPUProcessesFn(ctx, suspended.GPUPIDs, cuda.DefaultHelperBinaryPath, log); err != nil {
			log.Error(err, "Could not hand the GPUs back to the source")
			return "the GPU state could not be recovered", false
		}
	}

	// Then CRIU: a task still seized or stopped cannot resume on its own.
	if released, reason := snapshotruntime.Released(w.procRoot(), pids); !released {
		return reason, false
	}
	return "", true
}

// procRoot is where this agent reads the node's processes.
func (w *NodeController) procRoot() string {
	if w.hostProcPath == "" {
		return snapshotruntime.HostProcPath
	}
	return w.hostProcPath
}
