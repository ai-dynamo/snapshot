// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package executor provides the top-level checkpoint and restore executors.
// These wire together the lib packages (criu, cuda, etc.) into multi-step workflows.
package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// CheckpointRequest holds the content-owned inputs for a checkpoint operation.
type CheckpointRequest struct {
	ContainerID   string
	ContainerName string
	ContentUID    string
	StartedAt     time.Time
	NodeName      string
	PodName       string
	PodNamespace  string
	PodIP         string
	Clientset     kubernetes.Interface
}

type checkpointPhaseTimings struct {
	CUDACheckpointDuration time.Duration
	CRIUDumpDuration       time.Duration
	OverlayCaptureDuration time.Duration
}

type checkpointMutationError struct {
	err                error
	targetMayBeMutated bool
}

func (e *checkpointMutationError) Error() string { return e.err.Error() }
func (e *checkpointMutationError) Unwrap() error { return e.err }

func checkpointPreMutationError(err error) error {
	return &checkpointMutationError{err: err, targetMayBeMutated: false}
}

// CheckpointFailedBeforeTargetMutation is true only when checkpoint
// preflight failed before CRIU or any CUDA driver operation could mutate the
// source workload. Unclassified failures remain fail-closed.
func CheckpointFailedBeforeTargetMutation(err error) bool {
	var checkpointErr *checkpointMutationError
	return errors.As(err, &checkpointErr) && !checkpointErr.targetMayBeMutated
}

// Checkpoint performs a CRIU dump of a container.
//
// The checkpoint directory is staged under the content-owned .tmp directory.
// On success, the previous checkpoint is removed and the staged directory is
// renamed atomically into the content/container artifact path.
func Checkpoint(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req CheckpointRequest, cfg *types.AgentConfig) error {
	checkpointStart := time.Now()
	log.Info("=== Starting checkpoint operation ===")

	finalDir, err := nsmount.ResolveArtifactPath(cfg.Storage.BasePath, req.ContentUID, req.ContainerName)
	if err != nil {
		return checkpointPreMutationError(fmt.Errorf("resolve checkpoint artifact path: %w", err))
	}
	tmpRoot, err := nsmount.ResolveArtifactStagingRoot(cfg.Storage.BasePath, req.ContentUID)
	if err != nil {
		return checkpointPreMutationError(fmt.Errorf("resolve checkpoint staging root: %w", err))
	}
	if err := os.MkdirAll(tmpRoot, 0700); err != nil {
		return checkpointPreMutationError(fmt.Errorf("failed to create checkpoint staging root: %w", err))
	}
	if err := os.MkdirAll(filepath.Dir(finalDir), 0700); err != nil {
		return checkpointPreMutationError(fmt.Errorf("failed to create checkpoint container root: %w", err))
	}
	tmpDir := filepath.Join(tmpRoot, uuid.NewString())
	if err := os.Mkdir(tmpDir, 0700); err != nil {
		return checkpointPreMutationError(fmt.Errorf("failed to create checkpoint staging directory: %w", err))
	}
	defer os.RemoveAll(tmpDir)

	state, gpuDeviceMapDuration, err := inspectContainer(ctx, rt, log, req)
	if err != nil {
		return checkpointPreMutationError(err)
	}
	cudaJobFile := ""
	cudaStorageMode := types.CUDAStorageModeLegacy
	if len(state.CUDAHostPIDs) > 0 {
		cudaJobFile, err = cuda.StageJobFile(state.RootFS, tmpDir, len(state.GPUUUIDs))
		if err != nil {
			return checkpointPreMutationError(err)
		}
		if cfg.CUDACheckpoint.StorageMode == types.CUDAStorageModePOSIX {
			if err := validatePOSIXCustomStorageTopology(len(state.CUDAHostPIDs), len(state.GPUUUIDs)); err != nil {
				return checkpointPreMutationError(err)
			}
		}
		cudaStorageMode, err = cuda.SelectCUDAStorageMode(
			ctx,
			cfg.CUDACheckpoint.StorageMode,
		)
		if err != nil {
			return checkpointPreMutationError(fmt.Errorf("select CUDA storage mode before locking target: %w", err))
		}
		if cudaStorageMode == types.CUDAStorageModePOSIX {
			log.Info("CUDA CustomStorage explicitly enabled and available; using the Snapshot-local NIXL POSIX path",
				"cuda_storage_mode", cudaStorageMode)
		} else {
			log.Info("CUDA CustomStorage disabled for new checkpoints; using legacy CUDA checkpoint storage",
				"cuda_storage_mode", cudaStorageMode)
		}
	}

	criuOpts, data, err := configureCheckpoint(log, state, req, cfg, tmpDir, cudaStorageMode)
	if err != nil {
		return checkpointPreMutationError(err)
	}

	captureTimings, err := captureCheckpoint(
		ctx,
		criuOpts,
		&cfg.CRIU,
		cfg.CUDACheckpoint.TransferSettings(),
		data,
		state,
		tmpDir,
		cudaJobFile,
		cudaStorageMode,
		log,
	)
	if err != nil {
		return err
	}

	// Remove any previous checkpoint with the same identity hash, then
	// promote the staged checkpoint directory into place.
	switchStart := time.Now()
	if err := os.RemoveAll(finalDir); err != nil {
		return fmt.Errorf("failed to remove previous checkpoint directory: %w", err)
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return fmt.Errorf("failed to finalize checkpoint directory: %w", err)
	}
	switchDuration := time.Since(switchStart)

	wall := time.Since(checkpointStart)
	unaccounted := remainingDuration(wall,
		gpuDeviceMapDuration,
		captureTimings.CUDACheckpointDuration,
		captureTimings.CRIUDumpDuration,
		captureTimings.OverlayCaptureDuration,
		switchDuration,
	)
	summary := map[string]any{
		"duration": wall.String(),
		"phases": map[string]string{
			"gpu_device_map":                gpuDeviceMapDuration.String(),
			"cuda_checkpoint":               captureTimings.CUDACheckpointDuration.String(),
			"criu_dump":                     captureTimings.CRIUDumpDuration.String(),
			"overlay_capture":               captureTimings.OverlayCaptureDuration.String(),
			"remove_old_version_and_switch": switchDuration.String(),
			"unaccounted":                   unaccounted.String(),
		},
	}
	if !req.StartedAt.IsZero() {
		summary["started_to_complete"] = time.Since(req.StartedAt).String()
	}
	log.Info("Checkpoint timing summary", "checkpoint", summary)

	return nil
}

func validatePOSIXCustomStorageTopology(processCount, gpuCount int) error {
	if processCount < 1 || gpuCount != 1 {
		return fmt.Errorf("CUDA POSIX CustomStorage is qualified only for one or more CUDA processes on one GPU; found processes=%d GPUs=%d",
			processCount, gpuCount)
	}
	return nil
}

func inspectContainer(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
	containerID := req.ContainerID
	pid, ociSpec, err := rt.ResolveContainer(ctx, containerID)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to resolve container: %w", err)
	}

	var hostCgroupPath string
	if cgPath, err := snapshotruntime.ResolveCgroupRootFromHostPID(pid); err == nil && cgPath != "" {
		hostCgroupPath = filepath.Join(snapshotruntime.HostCgroupPath, cgPath)
	}

	rootFS, err := snapshotruntime.GetRootFS(pid)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get rootfs: %w", err)
	}

	upperDir, err := snapshotruntime.GetOverlayUpperDir(pid)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get overlay upperdir: %w", err)
	}

	mountInfo, err := snapshotruntime.ReadMountInfo(pid)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to parse mountinfo: %w", err)
	}
	mounts := snapshotruntime.ClassifyMounts(mountInfo, ociSpec, rootFS)

	netNSInode, err := snapshotruntime.GetNetNSInode(pid)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to get net namespace inode: %w", err)
	}

	// Read stdio FD targets (like runc's getPipeFds / descriptors.json).
	stdioFDs := make([]string, 3)
	for i := range 3 {
		target, err := os.Readlink(fmt.Sprintf("%s/%d/fd/%d", snapshotruntime.HostProcPath, pid, i))
		if err != nil {
			log.V(1).Info("Failed to readlink stdio FD", "fd", i, "error", err)
			continue
		}
		stdioFDs[i] = target
	}

	// Discover CUDA processes and GPU UUIDs
	allPIDs := snapshotruntime.ProcessTreePIDs(pid)
	cudaHostPIDs, err := cuda.FilterProcesses(ctx, allPIDs, log)
	if err != nil {
		return nil, 0, fmt.Errorf("discover CUDA processes: %w", err)
	}
	cudaNamespacePIDs := make([]int, 0, len(cudaHostPIDs))
	for _, cudaHostPID := range cudaHostPIDs {
		process, err := snapshotruntime.ReadProcessDetails(snapshotruntime.HostProcPath, cudaHostPID)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to read process details for CUDA process %d: %w", cudaHostPID, err)
		}
		if len(process.NamespacePIDs) != 2 {
			return nil, 0, fmt.Errorf("CUDA process %d has namespace depth %d, want 2", cudaHostPID, len(process.NamespacePIDs))
		}
		cudaNamespacePIDs = append(cudaNamespacePIDs, process.InnermostPID)
	}
	if len(cudaHostPIDs) > 0 {
		log.V(1).Info("Resolved checkpoint CUDA PID mapping", "host_pids", cudaHostPIDs, "namespace_pids", cudaNamespacePIDs)
	}
	var gpuUUIDs []string
	var gpuDeviceMapDuration time.Duration
	if len(cudaHostPIDs) > 0 {
		gpuStart := time.Now()
		gpuUUIDs, err = cuda.DiscoverGPUUUIDs(
			ctx,
			req.Clientset,
			req.PodName,
			req.PodNamespace,
			req.ContainerName,
			snapshotruntime.HostProcPath,
			pid,
			log,
		)
		gpuDeviceMapDuration = time.Since(gpuStart)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to discover source GPU UUIDs: %w", err)
		}
	}

	return &types.CheckpointContainerSnapshot{
		PID:            pid,
		RootFS:         rootFS,
		UpperDir:       upperDir,
		OCISpec:        ociSpec,
		Mounts:         mounts,
		NetNSInode:     netNSInode,
		StdioFDs:       stdioFDs,
		HostCgroupPath: hostCgroupPath,
		CUDAHostPIDs:   cudaHostPIDs,
		CUDANSPIDs:     cudaNamespacePIDs,
		GPUUUIDs:       gpuUUIDs,
	}, gpuDeviceMapDuration, nil
}

func configureCheckpoint(
	log logr.Logger,
	state *types.CheckpointContainerSnapshot,
	req CheckpointRequest,
	cfg *types.AgentConfig,
	checkpointDir string,
	cudaStorageMode string,
) (*criurpc.CriuOpts, *types.CheckpointManifest, error) {
	criuOpts, err := criu.BuildDumpOptions(state, &cfg.CRIU, checkpointDir, log)
	if err != nil {
		return nil, nil, err
	}

	m := types.NewCheckpointManifest(
		req.ContentUID,
		req.ContainerName,
		types.NewCRIUDumpManifest(criuOpts, cfg.CRIU),
		types.NewSourcePodManifest(req.ContainerID, state.PID, req.NodeName, req.PodName, req.PodNamespace, req.PodIP, state.StdioFDs),
		types.NewOverlayManifest(cfg.Overlay, state.UpperDir, state.OCISpec),
	)
	if len(state.CUDANSPIDs) > 0 {
		m.CUDA = types.NewCUDAManifest(state.CUDANSPIDs, state.GPUUUIDs, cudaStorageMode)
	}

	if err := types.WriteManifest(checkpointDir, m); err != nil {
		return nil, nil, fmt.Errorf("failed to write checkpoint manifest: %w", err)
	}

	return criuOpts, m, nil
}

func captureCheckpoint(
	ctx context.Context,
	criuOpts *criurpc.CriuOpts,
	criuSettings *types.CRIUSettings,
	cudaTransfer types.CUDATransferSettings,
	data *types.CheckpointManifest,
	state *types.CheckpointContainerSnapshot,
	checkpointDir,
	cudaJobFile,
	cudaStorageMode string,
	log logr.Logger,
) (*checkpointPhaseTimings, error) {
	timings := &checkpointPhaseTimings{}

	// CUDA lock+checkpoint must happen before CRIU dump
	if len(state.CUDAHostPIDs) > 0 {
		processes, err := readCUDAProcessDetailsForCheckpoint(
			snapshotruntime.HostProcPath,
			state.CUDAHostPIDs,
		)
		if err != nil {
			return nil, err
		}
		cudaTimings, err := cuda.LockAndCheckpointProcessTreeValidated(
			ctx,
			processes,
			cudaJobFile,
			cudaStorageMode,
			checkpointDir,
			state.GPUUUIDs,
			cudaTransfer,
			log,
		)
		if err != nil {
			checkpointErr := fmt.Errorf("CUDA checkpoint failed: %w", err)
			if cuda.FailedBeforeTargetMutation(err) {
				return nil, checkpointPreMutationError(checkpointErr)
			}
			cleanupErr := terminateCUDAProcessesAfterOperationFailure(
				processes,
				snapshotruntime.HostProcPath,
				"checkpoint",
				log,
				snapshotruntime.ValidateProcessIdentity,
				snapshotruntime.SendSignalToPID,
			)
			return nil, errors.Join(checkpointErr, cleanupErr)
		}
		timings.CUDACheckpointDuration = cudaTimings.TotalDuration
	}

	criuDumpDuration, err := criu.ExecuteDump(criuOpts, checkpointDir, criuSettings, log)
	if err != nil {
		return nil, err
	}
	timings.CRIUDumpDuration = criuDumpDuration

	// Overlay rootfs diff capture is best-effort. Failures are logged but not
	// propagated — a checkpoint without overlay diffs is still valid for restore
	// (the base container image provides the filesystem).
	if state.UpperDir != "" {
		overlayCaptureStart := time.Now()
		if _, err := snapshotruntime.CaptureRootfsDiff(state.UpperDir, checkpointDir, data.Overlay.Exclusions, data.Overlay.BindMountDests); err != nil {
			log.Error(err, "Failed to capture rootfs diff")
		}
		if _, err := snapshotruntime.CaptureDeletedFiles(state.UpperDir, checkpointDir); err != nil {
			log.Error(err, "Failed to capture deleted files")
		}
		timings.OverlayCaptureDuration = time.Since(overlayCaptureStart)
	}

	return timings, nil
}

func readCUDAProcessDetailsForCheckpoint(
	procRoot string,
	pids []int,
) ([]snapshotruntime.ProcessDetails, error) {
	processes := make([]snapshotruntime.ProcessDetails, 0, len(pids))
	for _, pid := range pids {
		process, err := snapshotruntime.ReadProcessDetails(procRoot, pid)
		if err != nil {
			return nil, checkpointPreMutationError(
				fmt.Errorf("capture CUDA process identity for PID %d: %w", pid, err),
			)
		}
		processes = append(processes, process)
	}
	return processes, nil
}

type signalProcessFunc func(logr.Logger, int, syscall.Signal, string) error

type validateProcessIdentityFunc func(string, snapshotruntime.ProcessDetails) error

func terminateCUDAProcessesAfterOperationFailure(
	processes []snapshotruntime.ProcessDetails,
	procRoot string,
	operation string,
	log logr.Logger,
	validateProcessIdentity validateProcessIdentityFunc,
	signalProcess signalProcessFunc,
) error {
	var cleanupErr error
	for _, process := range processes {
		pid := process.OutermostPID
		if err := validateProcessIdentity(procRoot, process); err != nil {
			cleanupErr = errors.Join(
				cleanupErr,
				fmt.Errorf(
					"refusing to terminate CUDA PID %d after %s identity validation failed: %w",
					pid,
					operation,
					err,
				),
			)
			continue
		}
		if err := signalProcess(log, pid, syscall.SIGKILL, "CUDA "+operation+" failed"); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if cleanupErr != nil {
		return fmt.Errorf("failed to terminate one or more CUDA processes after %s failure: %w", operation, cleanupErr)
	}
	return nil
}
