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
	"slices"
	"strings"
	"time"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const pageBrokerAbortTimeout = 30 * time.Second

// checkpointNeedsSourceKillError reports a failure after CUDA or CRIU may have left the source unsafe.
type checkpointNeedsSourceKillError struct{ cause error }

func (e *checkpointNeedsSourceKillError) Error() string { return e.cause.Error() }

func (e *checkpointNeedsSourceKillError) Unwrap() error { return e.cause }

func checkpointNeedsSourceKill(err error) error { return &checkpointNeedsSourceKillError{cause: err} }

// CheckpointNeedsSourceKill reports whether a failed checkpoint may have left the source unsafe.
func CheckpointNeedsSourceKill(err error) bool {
	var checkpointError *checkpointNeedsSourceKillError
	return errors.As(err, &checkpointError)
}

// CheckpointRequest holds the content-owned inputs for a checkpoint operation.
type CheckpointRequest struct {
	ContainerID         string
	ContainerName       string
	ContentUID          string
	StartedAt           time.Time
	NodeName            string
	PodName             string
	PodNamespace        string
	PodIP               string
	Clientset           kubernetes.Interface
	PageBrokerRequested bool

	// Pod carries the image reference and limits the target container runs with, read from
	// the live pod by the caller rather than here: the capture path has no API
	// client for the pod, and the reconciler already holds it.
	Pod compat.Environment
	// CuInterposeRequested is the workload's explicit opt-in. Capture then fails unless
	// every CUDA process loaded the shim. Without it, a loaded shim is still captured.
	CuInterposeRequested bool
}

type checkpointPhaseTimings struct {
	CUDACheckpointDuration time.Duration
	CRIUDumpDuration       time.Duration
	OverlayCaptureDuration time.Duration
}

// Checkpoint performs a CRIU dump of a container.
//
// The checkpoint directory is staged under the content-owned .tmp directory.
// On success, the previous checkpoint is removed and the staged directory is
// renamed atomically into the content/container artifact path.
func Checkpoint(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req CheckpointRequest, cfg *types.AgentConfig) error {
	return checkpoint(ctx, rt, log, req, cfg, inspectContainer)
}

func checkpoint(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req CheckpointRequest, cfg *types.AgentConfig,
	inspect func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error),
) (retErr error) {
	ctx = logr.NewContext(ctx, log)
	checkpointStart := time.Now()
	log.Info("=== Starting checkpoint operation ===")

	finalDir, err := nsmount.ResolveArtifactPath(cfg.Storage.BasePath, req.ContentUID, req.ContainerName)
	if err != nil {
		return fmt.Errorf("resolve checkpoint artifact path: %w", err)
	}
	state, gpuDeviceMapDuration, err := inspect(ctx, rt, log, req)
	if err != nil {
		return err
	}
	brokered := req.PageBrokerRequested && cfg.PageBroker.Enabled
	usePageBrokerGPU := brokered && cfg.CustomStorageAvailable && len(state.CUDAHostPIDs) > 0
	transactionID := uuid.NewString()
	var broker pagebroker.Client
	committed := false
	var tmpDir string
	var gpu *pagebroker.CustomStorageExecution
	if brokered {
		broker = pagebroker.Client{ControlSocketPath: cfg.PageBroker.ControlSocketPath}
		preparationAttempted := false
		defer func() {
			if gpu != nil {
				defer gpu.Close()
			}
			if preparationAttempted && !committed {
				if gpu != nil {
					if err := gpu.Abort(ctx); err != nil {
						retErr = errors.Join(retErr, fmt.Errorf("abort PageBroker checkpoint: %w", err))
					}
				} else {
					retErr = abortCheckpointTransaction(broker, transactionID, retErr)
				}
			}
		}()
		var executionContext *pagebroker.GpuContext
		if usePageBrokerGPU {
			executionContext, err = pagebroker.NewGPUContext(state.CUDANSPIDs, cuda.GPUUUIDs(state.GPUs), "")
			if err != nil {
				return err
			}
		}
		preparationAttempted = true
		tmpDir, gpu, err = prepareCheckpoint(ctx, broker, transactionID, finalDir, executionContext)
		if err != nil {
			return fmt.Errorf("prepare PageBroker checkpoint: %w", err)
		}
	} else {
		tmpRoot, err := nsmount.ResolveArtifactStagingRoot(cfg.Storage.BasePath, req.ContentUID)
		if err != nil {
			return fmt.Errorf("resolve checkpoint staging root: %w", err)
		}
		if err := os.MkdirAll(tmpRoot, 0700); err != nil {
			return fmt.Errorf("failed to create checkpoint staging root: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(finalDir), 0700); err != nil {
			return fmt.Errorf("failed to create checkpoint container root: %w", err)
		}
		tmpDir = filepath.Join(tmpRoot, transactionID)
		if err := os.Mkdir(tmpDir, 0700); err != nil {
			return fmt.Errorf("failed to create checkpoint staging directory: %w", err)
		}
		defer os.RemoveAll(tmpDir)
	}

	state.CuInterpose, err = inspectCuInterpose(ctx, state, req.CuInterposeRequested)
	if err != nil {
		return err
	}
	cudaJobFile := ""
	if len(state.CUDAHostPIDs) > 0 && !usePageBrokerGPU {
		cudaJobFile, err = cuda.StageJobFile(state.RootFS, tmpDir)
		if err != nil {
			return err
		}
		if err := cuda.CheckJobFile(cudaJobFile, len(state.GPUs.Devices), state.CuInterpose.UsesCoordinator()); err != nil {
			return err
		}
	}

	criuOpts, data, err := configureCheckpoint(log, state, req, cfg, tmpDir, usePageBrokerGPU)
	if err != nil {
		return err
	}

	captureTimings, err := captureCheckpoint(ctx, criuOpts, &cfg.CRIU, data, state, tmpDir, cudaJobFile, log, gpu)
	if err != nil {
		return checkpointNeedsSourceKill(err)
	}

	switchStart := time.Now()
	if brokered {
		if err := broker.Commit(ctx, transactionID); err != nil {
			return fmt.Errorf("commit PageBroker checkpoint: %w", err)
		}
		committed = true
	} else {
		// Remove any previous checkpoint with the same identity hash, then
		// promote the staged checkpoint directory into place.
		if err := os.RemoveAll(finalDir); err != nil {
			return fmt.Errorf("failed to remove previous checkpoint directory: %w", err)
		}
		if err := os.Rename(tmpDir, finalDir); err != nil {
			return fmt.Errorf("failed to finalize checkpoint directory: %w", err)
		}
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

func inspectContainer(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
	containerID := req.ContainerID
	pid, ociSpec, err := rt.ResolveContainer(ctx, containerID)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to resolve container: %w", err)
	}
	// Only the image-digest check reads this, and it treats a blank value as
	// unknown, so a runtime that cannot answer costs the comparison, not the
	// checkpoint.
	imageID, err := rt.ResolveContainerImageID(ctx, containerID)
	if err != nil {
		log.Error(err, "Failed to resolve the container image ID; this checkpoint will not record it",
			"containerID", containerID)
		imageID = ""
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
	cudaHostPIDs := cuda.FilterProcesses(ctx, allPIDs, log)
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
	var gpus compat.GPUInfo
	var gpuDevicePaths map[string]string
	var gpuDeviceMapDuration time.Duration
	if len(cudaHostPIDs) > 0 {
		gpuStart := time.Now()
		var env []string
		if ociSpec != nil && ociSpec.Process != nil {
			env = ociSpec.Process.Env
		}
		gpus, err = cuda.DiscoverGPUs(ctx, req.Clientset, req.PodName, req.PodNamespace,
			req.ContainerName, snapshotruntime.HostProcPath, pid, env, log)
		gpuDeviceMapDuration = time.Since(gpuStart)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to discover source GPU UUIDs: %w", err)
		}
		var gpuUUIDs []string
		for _, device := range gpus.Devices {
			gpuUUIDs = append(gpuUUIDs, device.UUID)
		}
		gpuDevicePaths, err = cuda.ResolveDevicePaths(snapshotruntime.HostProcPath, pid, gpuUUIDs)
		if err != nil {
			return nil, 0, err
		}
	}

	return &types.CheckpointContainerSnapshot{
		PID:            pid,
		ImageID:        imageID,
		RootFS:         rootFS,
		UpperDir:       upperDir,
		OCISpec:        ociSpec,
		Mounts:         mounts,
		NetNSInode:     netNSInode,
		StdioFDs:       stdioFDs,
		HostCgroupPath: hostCgroupPath,
		CUDAHostPIDs:   cudaHostPIDs,
		CUDANSPIDs:     cudaNamespacePIDs,
		GPUDevicePaths: gpuDevicePaths,
		GPUs:           gpus,
	}, gpuDeviceMapDuration, nil
}

// inspectCuInterpose verifies the shim libraries in every CUDA process and their
// delivery mount. When a coordinator is in use, it also checks every participant
// before capture becomes destructive.
func inspectCuInterpose(ctx context.Context, state *types.CheckpointContainerSnapshot, requested bool) (*types.CuInterposeManifest, error) {
	inspection, err := cuda.InspectCuInterposeLibraries(snapshotruntime.HostProcPath, state.CUDAHostPIDs, requested)
	if err != nil {
		return nil, err
	}
	if inspection == nil {
		return nil, nil
	}
	manifest := inspection.Manifest(state.CUDAHostPIDs, state.CUDANSPIDs)
	if err := checkCuInterposeMountReadOnly(state.Mounts); err != nil {
		return nil, err
	}
	if manifest.UsesCoordinator() {
		if err := cuda.InspectCuInterpose(ctx, coordinatorTarget(state.PID, manifest.PIDs)); err != nil {
			return nil, fmt.Errorf("inspect cuinterpose: %w", err)
		}
	}
	return manifest, nil
}

// checkCuInterposeMountReadOnly rejects a writable mount at or under the delivery
// path. Restore binds the agent's shared library directory at that path, and CRIU
// reapplies the source mount's flags to the bind.
func checkCuInterposeMountReadOnly(mounts []types.MountInfo) error {
	for _, m := range mounts {
		delivery := m.MountPoint == podcontract.CuInterposeMountPath ||
			strings.HasPrefix(m.MountPoint, podcontract.CuInterposeMountPath+"/")
		if delivery && !m.ReadOnly {
			return fmt.Errorf("cuinterpose delivery mount %s must be read-only", m.MountPoint)
		}
	}
	return nil
}

func coordinatorTarget(hostPID int, namespacePIDs []int) cuda.CuInterposeTarget {
	return cuda.CuInterposeTarget{
		ProcRoot:      snapshotruntime.HostProcPath,
		HostPID:       hostPID,
		NamespacePIDs: namespacePIDs,
		Binary:        cuda.DefaultCoordinatorBinaryPath,
	}
}

func configureCheckpoint(
	log logr.Logger,
	state *types.CheckpointContainerSnapshot,
	req CheckpointRequest,
	cfg *types.AgentConfig,
	checkpointDir string,
	customStorage bool,
) (*criurpc.CriuOpts, *types.CheckpointManifest, error) {
	criuOpts, err := criu.BuildDumpOptions(state, &cfg.CRIU, checkpointDir, log)
	if err != nil {
		return nil, nil, err
	}
	podEnvironment := req.Pod
	podEnvironment.ImageID = state.ImageID
	overlay := cfg.Overlay
	// GNU tar already skips sockets. The pattern keeps coordinator endpoints out of
	// the rootfs diff regardless.
	overlay.Exclusions = append(slices.Clone(overlay.Exclusions), cuda.CuInterposeSocketPattern)

	m := types.NewCheckpointManifest(
		req.ContentUID,
		req.ContainerName,
		types.NewCRIUDumpManifest(criuOpts, cfg.CRIU),
		types.NewSourcePodManifest(req.ContainerID, state.PID, req.NodeName, req.PodName, req.PodNamespace, req.PodIP, state.StdioFDs).
			WithPodEnvironment(podEnvironment),
		types.NewOverlayManifest(overlay, state.UpperDir, state.OCISpec),
		types.NewHostManifest(cfg.HostKernelVersion),
	)
	if len(state.CUDANSPIDs) > 0 {
		m.CUDA = types.NewCUDAManifest(state.CUDANSPIDs, state.GPUs)
		m.CUDA.CustomStorage = customStorage
		m.CUDA.DevicePaths = state.GPUDevicePaths
		if state.OCISpec != nil && state.OCISpec.Process != nil {
			m.CUDA.NVIDIAVisibleDevices = cuda.VisibleDevicesValue(state.OCISpec.Process.Env)
		}
	}
	m.CuInterpose = state.CuInterpose

	if err := types.WriteManifest(checkpointDir, m); err != nil {
		return nil, nil, fmt.Errorf("failed to write checkpoint manifest: %w", err)
	}

	return criuOpts, m, nil
}

func captureCheckpoint(ctx context.Context, criuOpts *criurpc.CriuOpts, criuSettings *types.CRIUSettings, data *types.CheckpointManifest, state *types.CheckpointContainerSnapshot, checkpointDir, cudaJobFile string, log logr.Logger, gpu *pagebroker.CustomStorageExecution) (*checkpointPhaseTimings, error) {
	timings := &checkpointPhaseTimings{}

	// GPU capture must finish before CRIU and filesystem capture.
	gpuDuration, err := captureGPU(ctx, data, state, checkpointDir, cudaJobFile, log, gpu)
	if err != nil {
		return nil, err
	}
	timings.CUDACheckpointDuration = gpuDuration

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

func captureGPU(ctx context.Context, data *types.CheckpointManifest, state *types.CheckpointContainerSnapshot,
	checkpointDir, cudaJobFile string, log logr.Logger, gpu *pagebroker.CustomStorageExecution,
) (time.Duration, error) {
	if len(state.CUDAHostPIDs) == 0 {
		return 0, nil
	}
	if data.CuInterpose.UsesCoordinator() {
		// Remove shared mappings before either CUDA checkpoint path locks the processes.
		if err := cuda.PrepareCuInterpose(ctx, coordinatorTarget(state.PID, data.CuInterpose.PIDs), checkpointDir); err != nil {
			return 0, fmt.Errorf("prepare cuinterpose: %w", err)
		}
	}
	if data.CUDA.CustomStorage {
		if gpu == nil {
			return 0, fmt.Errorf("missing PageBroker CustomStorage execution context")
		}
		pidfds, err := snapshotruntime.OpenProcessHandles(state.CUDAHostPIDs)
		if err != nil {
			return 0, err
		}
		defer func() {
			for _, file := range pidfds {
				file.Close()
			}
		}()
		start := time.Now()
		result, err := gpu.Checkpoint(ctx, state.CUDANSPIDs, state.CUDAHostPIDs, pidfds)
		if err != nil {
			return 0, fmt.Errorf("PageBroker GPU checkpoint: %w", err)
		}
		logGPUResult(log, result)
		return time.Since(start), nil
	}
	timings, err := cuda.CheckpointProcessTree(ctx, state.CUDAHostPIDs, cudaJobFile, checkpointDir, log)
	if err != nil {
		return 0, fmt.Errorf("CUDA checkpoint failed: %w", err)
	}
	return timings.TotalDuration, nil
}

func abortCheckpointTransaction(broker pagebroker.Client, transactionID string, cause error) error {
	abortCtx, cancel := context.WithTimeout(context.Background(), pageBrokerAbortTimeout)
	defer cancel()
	if err := broker.Abort(abortCtx, transactionID); err != nil {
		cause = errors.Join(cause, fmt.Errorf("abort PageBroker checkpoint %q: %w", transactionID, err))
	}
	return cause
}

func prepareCheckpoint(ctx context.Context, broker pagebroker.Client, transactionID, destination string, gpuContext *pagebroker.GpuContext) (string, *pagebroker.CustomStorageExecution, error) {
	if gpuContext == nil {
		directory, err := broker.PrepareCheckpoint(ctx, transactionID, destination)
		return directory, nil, err
	}
	return broker.PrepareGPUCheckpoint(ctx, transactionID, destination, gpuContext)
}

func logGPUResult(log logr.Logger, result *pagebroker.GpuComplete) {
	for _, participant := range result.GetParticipants() {
		log.Info("PageBroker GPU participant complete", "captured_pid", participant.CapturedPid,
			"bytes", participant.Bytes)
	}
}
