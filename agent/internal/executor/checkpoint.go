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
	"golang.org/x/sys/unix"
	"k8s.io/client-go/kubernetes"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	"github.com/ai-dynamo/snapshot/api/storage/coordination"
)

const pageBrokerAbortTimeout = 30 * time.Second

const dumpRetryDelayCeiling = 30 * time.Second

// Overridden in tests so a repeat does not wait out a real backoff.
var dumpRetryInitialDelay = 2 * time.Second

// Seams: the dump and the probe, so the repeat decision is testable without
// CRIU or a live process tree.
var (
	dumpCheckpointFunc = dumpCheckpoint
	dumpableFunc       = snapshotruntime.Dumpable
	snapshotTreeFunc   = snapshotruntime.SnapshotTree
)

// cleanupFailure marks a failure from releasing a transaction, as opposed to the
// capture failure it is reported alongside. The two travel joined, so without a
// mark a classifier can only tell them apart by the order they were joined in.
type cleanupFailure struct{ cause error }

func (e *cleanupFailure) Error() string { return e.cause.Error() }

func (e *cleanupFailure) Unwrap() error { return e.cause }

// CleanupFailure marks err as having come from cleanup rather than the capture.
func CleanupFailure(err error) error { return &cleanupFailure{cause: err} }

// IsCleanupFailure reports whether err came from releasing a transaction rather
// than from the capture itself.
func IsCleanupFailure(err error) bool {
	var cleanup *cleanupFailure
	return errors.As(err, &cleanup)
}

// sourceNeedsRecoveryError reports a failure after CUDA or CRIU had already touched the source,
// which leaves it suspended rather than running. What becomes of it is the caller's decision.
//
// It carries the GPU processes the capture found, so recovery hands back exactly those rather
// than probing for them again: the probe cannot tell "not a CUDA process" from a helper that
// failed, and reading an empty list as "nothing to revive" would strand a suspended workload.
type sourceNeedsRecoveryError struct {
	cause     error
	suspended SuspendedSource
}

func (e *sourceNeedsRecoveryError) Error() string { return e.cause.Error() }

func (e *sourceNeedsRecoveryError) Unwrap() error { return e.cause }

func sourceNeedsRecovery(err error, cudaHostPIDs []int, cuInterposePrepared, customStorageUsed bool, dumpRetriesUsed int) error {
	return &sourceNeedsRecoveryError{cause: err, suspended: SuspendedSource{
		GPUPIDs:             slices.Clone(cudaHostPIDs),
		CuInterposePrepared: cuInterposePrepared,
		CustomStorageUsed:   customStorageUsed,
		DumpRetriesUsed:     dumpRetriesUsed,
	}}
}

// SuspendedSource describes what a failed capture left behind, so the caller can decide
// whether the workload can be put back.
type SuspendedSource struct {
	// GPUPIDs are the GPU processes the capture found, to be handed their GPUs back.
	GPUPIDs []int

	// CuInterposePrepared reports that the capture removed the tree's shared GPU mappings.
	// Nothing puts them back, so a source in that state can only be terminated.
	CuInterposePrepared bool

	// CustomStorageUsed reports that the GPU memory went to PageBroker rather than staying
	// with the driver. That execution is torn down before recovery runs, so an in-place
	// restore has nothing to restore from and the source can only be terminated.
	CustomStorageUsed bool

	// DumpRetriesUsed is how much of the retry budget this attempt already spent repeating
	// its dump, so one budget covers repeats and fresh attempts alike.
	DumpRetriesUsed int
}

// SuspendedSourceFailure marks err as having left the source suspended, carrying what
// recovery needs to decide what becomes of it.
func SuspendedSourceFailure(err error, suspended SuspendedSource) error {
	return &sourceNeedsRecoveryError{cause: err, suspended: suspended}
}

// SuspendedSourceOf returns what a failed capture left behind, and whether the failure left
// the source needing recovery at all.
func SuspendedSourceOf(err error) (SuspendedSource, bool) {
	var recoverable *sourceNeedsRecoveryError
	if !errors.As(err, &recoverable) {
		return SuspendedSource{}, false
	}
	return recoverable.suspended, true
}

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

	// StoreID binds this capture to a configured store; empty means legacy.
	StoreID string

	// DumpRetriesRemaining is what is left of the capture's retry budget, spent repeating
	// the dump while the workload is still suspended and still dumpable.
	DumpRetriesRemaining int

	// DumpWorthRepeating reports whether repeating could reach a different outcome,
	// and when it could not, why. It is asked only after a dump has failed. Nil
	// repeats whenever the workload is still dumpable.
	DumpWorthRepeating func(err error) (bool, string)

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
	// DumpRetries counts repeats of the dump against the same suspended tree.
	DumpRetries int
}

// Checkpoint captures a container through PageBroker and commits its artifact.
// The broker owns storage publication and, when supported, CUDA CustomStorage.
func Checkpoint(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req CheckpointRequest, cfg *types.AgentConfig) (*pagebroker.PublishedArtifact, error) {
	return checkpoint(ctx, rt, log, req, cfg, inspectContainer)
}

func checkpoint(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req CheckpointRequest, cfg *types.AgentConfig,
	inspect func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error),
) (artifact *pagebroker.PublishedArtifact, retErr error) {
	ctx = logr.NewContext(ctx, log)
	checkpointStart := time.Now()
	log.Info("=== Starting checkpoint operation ===")

	bound := req.StoreID != ""
	var target *pagebroker.ArtifactTarget
	var finalDir string
	if bound {
		if err := coordination.ValidateBinding(req.StoreID, req.ContentUID, req.ContainerName); err != nil {
			return nil, failStep(StepResolveCheckpointPath, fmt.Errorf("validate storage binding: %w", err))
		}
		target = &pagebroker.ArtifactTarget{
			StoreId: req.StoreID,
			Artifact: &pagebroker.ArtifactIdentity{
				ArtifactUid:   req.ContentUID,
				ContainerName: req.ContainerName,
			},
		}
	} else {
		var err error
		finalDir, err = nsmount.ResolveArtifactPath(cfg.Storage.BasePath, req.ContentUID, req.ContainerName)
		if err != nil {
			return nil, failStep(StepResolveCheckpointPath, err)
		}
	}
	state, gpuDeviceMapDuration, err := inspect(ctx, rt, log, req)
	if err != nil {
		return nil, failStep(StepInspectSource, err)
	}
	usePageBrokerGPU := cfg.CustomStorageAvailable && len(state.CUDAHostPIDs) > 0
	transactionID := uuid.NewString()
	broker := pagebroker.Client{ControlSocketPath: cfg.PageBroker.ControlSocketPath}
	committed := false
	preparationAttempted := false
	dumpComplete := false
	var gpu *pagebroker.CustomStorageExecution
	defer func() {
		if gpu != nil {
			defer gpu.Close()
		}
		if !preparationAttempted || committed {
			return
		}
		// gpu.Abort is also that execution's teardown, so the GPU path always
		// releases. A dial failure is no answer from the broker, so a completed
		// dump is kept.
		if gpu == nil && dumpComplete &&
			(pagebroker.CommitWorthRetrying(retErr) || pagebroker.IsDialError(retErr)) {
			return
		}
		if gpu != nil {
			if err := gpu.Abort(ctx); err != nil {
				retErr = errors.Join(retErr, CleanupFailure(fmt.Errorf("abort PageBroker checkpoint: %w", err)))
			}
		} else {
			retErr = abortCheckpointTransaction(broker, transactionID, retErr)
		}
	}()
	var executionContext *pagebroker.GpuContext
	if usePageBrokerGPU {
		executionContext, err = pagebroker.NewGPUContext(state.CUDANSPIDs, cuda.GPUUUIDs(state.GPUs), "")
		if err != nil {
			return nil, failStep(StepBuildGPUContext, err)
		}
	}
	preparationAttempted = true
	var tmpDir string
	tmpDir, gpu, err = prepareCheckpointTarget(ctx, broker, transactionID, bound, target, finalDir, executionContext)
	if err != nil {
		preparationAttempted = !pagebroker.IsDialError(err)
		return nil, failStep(StepPrepareCheckpoint, err)
	}

	state.CuInterpose, err = inspectCuInterpose(ctx, state, req.CuInterposeRequested)
	if err != nil {
		return nil, failStep(StepInspectCuInterpose, err)
	}
	cudaJobFile := ""
	if len(state.CUDAHostPIDs) > 0 && !usePageBrokerGPU {
		cudaJobFile, err = cuda.StageJobFile(state.RootFS, tmpDir)
		if err != nil {
			return nil, failStep(StepStageCUDAJobFile, err)
		}
		if err := cuda.CheckJobFile(cudaJobFile, len(state.GPUs.Devices), state.CuInterpose.UsesCoordinator()); err != nil {
			return nil, failStep(StepStageCUDAJobFile, err)
		}
	}

	criuOpts, data, err := configureCheckpoint(log, state, req, cfg, tmpDir, usePageBrokerGPU)
	if err != nil {
		return nil, failStep(StepConfigureCheckpoint, err)
	}

	captureTimings, err := captureCheckpointFunc(ctx, criuOpts, &cfg.CRIU, data, state, tmpDir, cudaJobFile, log, gpu, req.DumpRetriesRemaining, req.DumpWorthRepeating)
	if err != nil {
		repeats := 0
		if captureTimings != nil {
			repeats = captureTimings.DumpRetries
		}
		return nil, sourceNeedsRecovery(err, state.CUDAHostPIDs, data.CuInterpose.UsesCoordinator(), usePageBrokerGPU, repeats)
	}

	dumpComplete = true

	switchStart := time.Now()
	artifact, err = commitCheckpointArtifact(ctx, broker, transactionID, bound, req.StoreID)
	if err != nil {
		return nil, failStep(StepCommitCheckpoint, err)
	}
	committed = true
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

	return artifact, nil
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
		// One nvidia-smi answers both the MIG check and GPU discovery.
		visible := cuda.ListVisibleGPUs(ctx, hostProcPath, pid, ociSpec)
		if err := refuseMIGCapture(log, pid, visible); err != nil {
			return nil, 0, err
		}
		var env []string
		if ociSpec != nil && ociSpec.Process != nil {
			env = ociSpec.Process.Env
		}
		gpus, err = cuda.DiscoverGPUs(ctx, req.Clientset, req.PodName, req.PodNamespace,
			req.ContainerName, env, visible, log)
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
		types.NewHostManifest(cfg.HostKernelVersion, cfg.HostCPUFeatures),
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

// DumpSpaceError names the directory a dump ran out of room in. PageBroker
// chooses that directory, so it is not always the node's staging volume, and a
// caller measuring the wrong filesystem would reach the wrong conclusion about
// whether retrying could ever help.
type DumpSpaceError struct {
	Dir string
	Err error
}

func (e *DumpSpaceError) Error() string { return e.Err.Error() }
func (e *DumpSpaceError) Unwrap() error { return e.Err }

// withDumpDirectory records where a dump was writing when it filled the volume.
func withDumpDirectory(err error, dir string) error {
	if err == nil || !errors.Is(err, unix.ENOSPC) {
		return err
	}
	return &DumpSpaceError{Dir: dir, Err: err}
}

// captureCheckpointFunc is overridden in tests: captureCheckpoint runs CRIU
// against a live process tree, which a unit test cannot satisfy.
var captureCheckpointFunc = captureCheckpoint

func captureCheckpoint(ctx context.Context, criuOpts *criurpc.CriuOpts, criuSettings *types.CRIUSettings, data *types.CheckpointManifest, state *types.CheckpointContainerSnapshot, checkpointDir, cudaJobFile string, log logr.Logger, gpu *pagebroker.CustomStorageExecution, retriesRemaining int, worthRepeating func(error) (bool, string)) (*checkpointPhaseTimings, error) {
	timings := &checkpointPhaseTimings{}

	// GPU capture must finish before CRIU and filesystem capture.
	gpuDuration, err := captureGPU(ctx, data, state, checkpointDir, cudaJobFile, log, gpu)
	if err != nil {
		return nil, failStep(StepCheckpointGPU, err)
	}
	timings.CUDACheckpointDuration = gpuDuration

	// Recorded once the workload is suspended, so a later probe compares against
	// the tree the dump was meant to capture. Only a capture that may repeat pays
	// for it: recording walks /proc for every task, with the workload suspended.
	var recordedTree snapshotruntime.TreeSnapshot
	if retriesRemaining > 0 {
		if recordedTree, err = snapshotTreeFunc(snapshotruntime.HostProcPath,
			snapshotruntime.ProcessTreePIDs(state.PID)); err != nil {
			log.Info("Cannot record the process tree; the dump will not be repeated", "error", err.Error())
		}
	}

	for attempt := 0; ; attempt++ {
		err := withDumpDirectory(dumpCheckpointFunc(ctx, criuOpts, criuSettings, data, state, checkpointDir, timings, log), checkpointDir)
		if err == nil {
			return timings, nil
		}
		if recordedTree == nil {
			return timings, err
		}
		if attempt >= retriesRemaining {
			return timings, err
		}
		// Asked before the tree is probed: a dump nothing could make fit is not worth
		// repeating however seizable the workload still is.
		if worthRepeating != nil {
			if ok, reason := worthRepeating(err); !ok {
				log.V(1).Info("Not repeating the dump", "reason", reason)
				return timings, err
			}
		}
		// The probe decides, not the shape of the failure: the suspend is already
		// paid for, and repeating the dump is only safe while CRIU can still seize
		// the tree it left behind.
		dumpable, reason := dumpableFunc(snapshotruntime.HostProcPath, recordedTree)
		if !dumpable {
			log.V(1).Info("Not repeating the dump", "reason", reason)
			return timings, err
		}
		select {
		case <-ctx.Done():
			return timings, err
		case <-time.After(dumpRetryDelay(attempt)):
		}
		// select picks at random when the wait and the cancellation are both ready,
		// so the deadline gets the last word rather than one dump in two.
		if ctx.Err() != nil {
			return timings, err
		}
		// Counted as the repeat's dump starts, so a capture cancelled in the backoff
		// does not spend a retry it never took.
		timings.DumpRetries++
		log.Info("Repeating the dump against the suspended workload", "attempt", timings.DumpRetries, "error", err.Error())
	}
}

// dumpRetryDelay spaces repeats so a node momentarily out of staging has time to
// free some, without letting the gap grow past what a capture can spend waiting.
func dumpRetryDelay(attempt int) time.Duration {
	// Far enough out the shift overflows int64, and a delay that lands on zero or
	// negative makes time.After fire at once — repeats with no backoff at all.
	delay := dumpRetryInitialDelay << attempt
	if delay <= 0 || delay > dumpRetryDelayCeiling {
		return dumpRetryDelayCeiling
	}
	return delay
}

// dumpCheckpoint writes the image of a workload that is already suspended.
//
// Separate from the suspend because only the suspend is expensive: it moves GPU
// memory to host memory, and a tree that is still dumpable can have its image
// written again without paying that twice.
func dumpCheckpoint(
	ctx context.Context,
	criuOpts *criurpc.CriuOpts,
	criuSettings *types.CRIUSettings,
	data *types.CheckpointManifest,
	state *types.CheckpointContainerSnapshot,
	checkpointDir string,
	timings *checkpointPhaseTimings,
	log logr.Logger,
) error {
	criuDumpDuration, err := criu.ExecuteDump(criuOpts, checkpointDir, criuSettings, log)
	if err != nil {
		return failStep(StepDumpCRIU, err)
	}
	timings.CRIUDumpDuration = criuDumpDuration

	// Recorded from the images CRIU wrote, so the manifest states what the
	// dump did rather than what the node was expected to allow.
	data.CRIUDump.LSMLabels, err = criu.RecordedLSMLabels(checkpointDir)
	if err != nil {
		return fmt.Errorf("read the dump's security labels: %w", err)
	}
	if err := types.WriteManifest(checkpointDir, data); err != nil {
		return fmt.Errorf("failed to write checkpoint manifest: %w", err)
	}

	if state.UpperDir != "" {
		overlayCaptureStart := time.Now()
		if err := captureOverlay(state.UpperDir, data.Overlay, checkpointDir, log); err != nil {
			return failStep(StepCaptureOverlay, err)
		}
		timings.OverlayCaptureDuration = time.Since(overlayCaptureStart)
	}

	return nil
}

// captureOverlay records the upperdir's rootfs diff and deleted files for a
// dump in checkpointDir.
//
// Overlay rootfs diff capture is best-effort. Failures are logged but not
// propagated — a checkpoint without overlay diffs is still valid for restore
// (the base container image provides the filesystem). Two failures are fatal
// instead, because restore would then fail: a dump whose referenced files
// cannot be read, since restore decodes the same images, and a rootfs diff
// that may lack a referenced upperdir file, which restore reopens.
func captureOverlay(upperDir string, overlay types.OverlayManifest, checkpointDir string, log logr.Logger) error {
	referenced, err := criu.ReadReferencedPaths(checkpointDir)
	if err != nil {
		return fmt.Errorf("read the files the dump references: %w", err)
	}
	diff, err := snapshotruntime.CaptureRootfsDiff(upperDir, checkpointDir, snapshotruntime.RootfsDiffOptions{
		Exclusions:     overlay.Exclusions,
		BindMountDests: overlay.BindMountDests,
		Keep:           referenced,
	})
	if errors.Is(err, snapshotruntime.ErrKeptFilesMissing) {
		return fmt.Errorf("capture rootfs diff: %w", err)
	}
	if err != nil {
		log.Error(err, "Failed to capture rootfs diff")
	} else if len(diff.Kept) > 0 {
		log.Info("Kept referenced files matched by rootfs exclusions",
			"count", len(diff.Kept), "bytes", diff.KeptBytes, "paths", diff.Kept[:min(len(diff.Kept), 20)])
	}
	if _, err := snapshotruntime.CaptureDeletedFiles(upperDir, checkpointDir); err != nil {
		log.Error(err, "Failed to capture deleted files")
	}
	return nil
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

// abortPageBrokerTransaction runs Abort with the shared release timeout.
func abortPageBrokerTransaction(broker pagebroker.Client, transactionID string) error {
	abortCtx, cancel := context.WithTimeout(context.Background(), pageBrokerAbortTimeout)
	defer cancel()
	return broker.Abort(abortCtx, transactionID)
}

func abortCheckpointTransaction(broker pagebroker.Client, transactionID string, cause error) error {
	if err := abortPageBrokerTransaction(broker, transactionID); err != nil {
		cause = errors.Join(cause, CleanupFailure(fmt.Errorf("abort PageBroker checkpoint %q: %w", transactionID, err)))
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

// prepareCheckpointTarget prepares the PageBroker transaction for a bound (artifact-addressed)
// or legacy (filesystem) checkpoint destination.
func prepareCheckpointTarget(ctx context.Context, broker pagebroker.Client, transactionID string, bound bool, target *pagebroker.ArtifactTarget, destination string, gpuContext *pagebroker.GpuContext) (string, *pagebroker.CustomStorageExecution, error) {
	switch {
	case bound && gpuContext != nil:
		return broker.PrepareGPUArtifactCheckpoint(ctx, transactionID, target, nil, gpuContext)
	case bound:
		directory, err := broker.PrepareArtifactCheckpoint(ctx, transactionID, target, nil)
		return directory, nil, err
	default:
		return prepareCheckpoint(ctx, broker, transactionID, destination, gpuContext)
	}
}

// commitCheckpointArtifact commits the PageBroker transaction, publishing an artifact for a
// bound checkpoint or finalizing the legacy filesystem destination otherwise.
func commitCheckpointArtifact(ctx context.Context, broker pagebroker.Client, transactionID string, bound bool, storeID string) (*pagebroker.PublishedArtifact, error) {
	if bound {
		return broker.CommitCheckpoint(ctx, transactionID, storeID)
	}
	return nil, broker.Commit(ctx, transactionID)
}

func logGPUResult(log logr.Logger, result *pagebroker.GpuComplete) {
	for _, participant := range result.GetParticipants() {
		log.Info("PageBroker GPU participant complete", "captured_pid", participant.CapturedPid,
			"bytes", participant.Bytes)
	}
}
