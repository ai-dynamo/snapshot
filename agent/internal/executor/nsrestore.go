// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// RestoreOptions holds configuration for an in-namespace restore.
type RestoreOptions struct {
	CheckpointPath string
	CUDADeviceMap  string
	CgroupRoot     string
	TargetPodIP    string
	// BundleDir is the path where the agent's binary bundle is mounted inside this namespace.
	BundleDir string
}

type RestoreInNamespaceResult struct {
	RestoredPID            int                              `json:"restoredPID"`
	CleanupError           *CleanupError                    `json:"cleanupError,omitempty"`
	OverlayCaptureDuration time.Duration                    `json:"overlayCaptureDuration"`
	CRIUPrepareDuration    time.Duration                    `json:"criuPrepareDuration"`
	CRIURestoreDuration    time.Duration                    `json:"criuRestoreDuration"`
	CUDARestoreDuration    time.Duration                    `json:"cudaRestoreDuration"`
	DeferredCUDAProcesses  []snapshotruntime.ProcessDetails `json:"deferredCUDAProcesses,omitempty"`
}

// CleanupError is the wire representation of a successful restore whose
// in-namespace cleanup did not fully complete.
type CleanupError struct {
	Action  string `json:"action"`
	Message string `json:"message"`
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("%s: %s", e.Action, e.Message)
}

// RestoreInNamespace performs a full restore from inside the target container's namespaces.
func RestoreInNamespace(ctx context.Context, opts RestoreOptions, log logr.Logger) (*RestoreInNamespaceResult, error) {
	log.Info("Starting nsrestore workflow",
		"checkpoint_path", opts.CheckpointPath,
		"has_cuda_map", opts.CUDADeviceMap != "",
		"cgroup_root", opts.CgroupRoot,
		"target_pod_ip_present", opts.TargetPodIP != "",
	)
	m, err := types.ReadManifest(opts.CheckpointPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest: %w", err)
	}
	log.V(1).Info("Loaded checkpoint manifest",
		"ext_mounts", len(m.CRIUDump.ExtMnt),
		"criu_log_level", m.CRIUDump.CRIU.LogLevel,
		"manage_cgroups_mode", m.CRIUDump.CRIU.ManageCgroupsMode,
		"checkpoint_has_cuda", !m.CUDA.IsEmpty(),
	)
	cudaJobFile := ""
	if !m.CUDA.IsEmpty() {
		cudaJobFile, err = cuda.JobFileFromCheckpoint(opts.CheckpointPath)
		if err != nil {
			return nil, err
		}
		if len(m.CUDA.SourceGPUUUIDs) > 1 && cudaJobFile == "" {
			return nil, fmt.Errorf("multi-GPU checkpoint is missing CUDA launch-job state")
		}
	}

	if err := criu.ConfigureInetRemap(m, opts.TargetPodIP, log); err != nil {
		return nil, err
	}
	criuOpts, err := criu.BuildRestoreOpts(m, opts.CheckpointPath, opts.CgroupRoot, log)
	if err != nil {
		return nil, err
	}

	executeTimings, restoredPID, cleanupErr, err := executeRestore(ctx, criuOpts, m, opts, cudaJobFile, log)
	if err != nil {
		return nil, err
	}

	result := &RestoreInNamespaceResult{
		RestoredPID:            restoredPID,
		OverlayCaptureDuration: executeTimings.overlayCaptureDuration,
		CRIUPrepareDuration:    executeTimings.criuPrepareDuration,
		CRIURestoreDuration:    executeTimings.criuRestoreDuration,
		CUDARestoreDuration:    executeTimings.cudaRestoreDuration,
		DeferredCUDAProcesses:  executeTimings.deferredCUDAProcesses,
	}
	if cleanupErr != nil {
		result.CleanupError = &CleanupError{
			Action:  "clean CRIU restore resources",
			Message: cleanupErr.Error(),
		}
	}
	return result, nil
}

type nsrestorePhaseTimings struct {
	overlayCaptureDuration time.Duration
	criuPrepareDuration    time.Duration
	criuRestoreDuration    time.Duration
	cudaRestoreDuration    time.Duration
	deferredCUDAProcesses  []snapshotruntime.ProcessDetails
}

func executeRestore(
	ctx context.Context,
	criuOpts *criurpc.CriuOpts,
	m *types.CheckpointManifest,
	opts RestoreOptions,
	cudaJobFile string,
	log logr.Logger,
) (timings *nsrestorePhaseTimings, restoredPID int, cleanupErr error, retErr error) {
	timings = &nsrestorePhaseTimings{}

	overlayStart := time.Now()
	if err := snapshotruntime.ApplyRootfsDiff(opts.CheckpointPath, "/", log); err != nil {
		return nil, 0, nil, fmt.Errorf("rootfs diff failed: %w", err)
	}
	if err := snapshotruntime.ApplyDeletedFiles(opts.CheckpointPath, "/", log); err != nil {
		log.Error(err, "Failed to apply deleted files")
	}
	timings.overlayCaptureDuration = time.Since(overlayStart)
	cudaRestoreJobFile := ""
	if cudaJobFile != "" {
		liveJobFile, err := cuda.PrepareLiveJobFile(cudaJobFile)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("prepare CUDA checkpoint job file: %w", err)
		}
		cudaRestoreJobFile = liveJobFile
		if err := os.Setenv(cuda.JobFileEnv, cudaRestoreJobFile); err != nil {
			return nil, 0, nil, fmt.Errorf("set CUDA checkpoint job file environment: %w", err)
		}
	}

	// Unmount placeholder's /dev/shm so CRIU can recreate tmpfs with checkpointed content
	if err := syscall.Unmount("/dev/shm", 0); err != nil {
		return nil, 0, nil, fmt.Errorf("failed to unmount /dev/shm before restore: %w", err)
	}

	if err := snapshotruntime.RemountProcSys(true); err != nil {
		return nil, 0, nil, fmt.Errorf("failed to remount /proc/sys read-write for restore: %w", err)
	}
	defer func() {
		if err := snapshotruntime.RemountProcSys(false); err != nil {
			log.Error(err, "Failed to remount /proc/sys read-only after restore")
		}
	}()

	// The restore-complete sentinel lives on the pod emptyDir mounted at
	// SnapshotControlMountPath. Clear it here, in that mount namespace, so a
	// leftover from an earlier incarnation cannot release the restored process
	// before this CRIU/CUDA attempt finishes. A missing file is already gone;
	// a missing mount is a hard error.
	if err := snapshotruntime.RemoveControlSentinel(snapshotv1alpha1.SnapshotControlMountPath, snapshotv1alpha1.RestoreCompleteFile); err != nil {
		return nil, 0, nil, fmt.Errorf("remove stale restore-complete sentinel: %w", err)
	}

	criuPID, cleanup, prepare, restore, err := criu.ExecuteRestore(criuOpts, m, opts.CheckpointPath, opts.BundleDir, log)
	if err != nil {
		return nil, 0, nil, err
	}
	restoredPID = int(criuPID)
	// Cleanup releases only CRIU-owned files and scratch paths. It runs when
	// nsrestore returns, before the host agent asks the CUDA daemon to restore
	// and unlock the parked processes. A cleanup-only failure is returned
	// separately so the host controller can warn without killing the workload.
	defer func() {
		if err := cleanup(); err != nil {
			log.Error(err, "failed to clean CRIU restore resources")
			if retErr == nil {
				cleanupErr = err
			}
		}
	}()
	timings.criuPrepareDuration = prepare
	timings.criuRestoreDuration = restore

	if cudaRestoreJobFile != "" {
		uid, gid, err := snapshotruntime.ReadProcessFilesystemIDs("/proc", restoredPID)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("read restored process credentials: %w", err)
		}
		if err := cuda.SetLiveJobFileOwner(cudaRestoreJobFile, uid, gid); err != nil {
			return nil, 0, nil, fmt.Errorf("set CUDA checkpoint job file ownership: %w", err)
		}
	}
	processes, err := snapshotruntime.ReadProcessTable("/proc")
	if err != nil {
		return nil, 0, nil, fmt.Errorf("failed to read restored process table: %w", err)
	}
	log.V(1).Info("Restored process table snapshot",
		"proc_root", "/proc",
		"criu_callback_pid", restoredPID,
		"process_count", len(processes),
		"manifest_cuda_pids", m.CUDA.PIDs,
	)
	for _, process := range processes {
		log.V(1).Info("Restored process entry",
			"observed_pid", process.ObservedPID,
			"parent_pid", process.ParentPID,
			"outermost_pid", process.OutermostPID,
			"innermost_pid", process.InnermostPID,
			"namespace_pids", process.NamespacePIDs,
			"cmdline", process.Cmdline,
		)
	}

	// Resolve the restored CUDA processes while still inside the restored PID
	// namespace. The host agent maps these identities to host PIDs and delegates
	// restore to the always-on CUDA helper daemon after nsrestore returns.
	if !m.CUDA.IsEmpty() {
		restorePIDs, err := snapshotruntime.ResolveManifestPIDsToObservedPIDs(processes, restoredPID, m.CUDA.PIDs)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("failed to resolve restored CUDA PIDs: %w", err)
		}
		log.V(1).Info("Resolved manifest CUDA PIDs to current restore PIDs",
			"manifest_cuda_pids", m.CUDA.PIDs,
			"restored_cuda_pids", restorePIDs,
			"criu_callback_pid", restoredPID,
		)
		for _, pid := range restorePIDs {
			process, err := snapshotruntime.ReadProcessDetails("/proc", pid)
			if err != nil {
				return nil, 0, nil, fmt.Errorf("capture restored CUDA process identity for PID %d: %w", pid, err)
			}
			timings.deferredCUDAProcesses = append(timings.deferredCUDAProcesses, process)
		}
	}

	return timings, restoredPID, nil, nil
}
