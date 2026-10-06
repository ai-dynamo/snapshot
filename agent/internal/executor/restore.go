// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/client-go/kubernetes"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/logging"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const failedRestoreTerminationTimeout = 30 * time.Second

// RestoreMounter installs the fixed binary bundle, the cuinterpose libraries and one
// validated checkpoint artifact inside a placeholder container's mount namespace.
type RestoreMounter interface {
	MountBundle(ctx context.Context, pid int) (nsmount.MountPoint, error)
	MountCuInterpose(ctx context.Context, namespaceMount nsmount.MountPoint) (nsmount.MountPoint, error)
	MountArtifact(ctx context.Context, namespaceMount nsmount.MountPoint, artifactPath string) (nsmount.MountPoint, error)
	MountPageBroker(ctx context.Context, namespaceMount nsmount.MountPoint, stagingPath string) (nsmount.MountPoint, error)
}

// prepareGPUMapping produces the plan used by both mount inspection and nsrestore.
// Compatibility policy remains in the registered checks, not in this preparation.
func prepareGPUMapping(log logr.Logger, manifest *types.CheckpointManifest, uuids []string,
	resolvePaths func() (map[string]string, error),
) (string, map[string]string, error) {
	// Let the compatibility gate report count mismatches before positional pairing.
	if len(uuids) == 0 || len(uuids) != len(manifest.CUDA.SourceGPUUUIDs) {
		return "", nil, nil
	}
	deviceMap, err := cuda.BuildDeviceMap(manifest.CUDA.SourceGPUUUIDs, uuids, log)
	if err != nil {
		return "", nil, err
	}
	if len(manifest.CUDA.DevicePaths) == 0 {
		return deviceMap, nil, nil
	}
	paths, err := resolvePaths()
	if err != nil {
		return "", nil, err
	}
	aliases, err := criu.GPUMountAliases(manifest, deviceMap, paths)
	return deviceMap, aliases, err
}

// RestoreCleanupError reports a successful restore whose cleanup did not fully
// complete. The controller logs it, emits a warning event, and completes restore.
type RestoreCleanupError struct {
	Err error
}

func NewRestoreCleanupError(err error) *RestoreCleanupError {
	return &RestoreCleanupError{Err: err}
}

func (e *RestoreCleanupError) Error() string { return e.Err.Error() }
func (e *RestoreCleanupError) Unwrap() error { return e.Err }

type restoreMount struct {
	action        string
	point         nsmount.MountPoint
	keepOnSuccess bool
}

func cleanupRestoreMounts(ctx context.Context, mounts []restoreMount, restored bool) error {
	var cleanupErr error
	cleanupCtx := context.WithoutCancel(ctx)
	for i := len(mounts) - 1; i >= 0; i-- {
		if restored && mounts[i].keepOnSuccess {
			if err := mounts[i].point.Release(); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("release retained mount namespace: %w", err))
			}
			continue
		}
		if err := mounts[i].point.Unmount(cleanupCtx); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", mounts[i].action, err))
		}
	}
	return cleanupErr
}

// RestoreRequest holds the parameters for a restore operation.
type RestoreRequest struct {
	ContentUID                  string
	BasePath                    string
	ContainerID                 string
	StartedAt                   time.Time
	PodName                     string
	PodNamespace                string
	TargetPodIP                 string
	ArtifactContainerName       string
	DestinationContainerName    string
	Clientset                   kubernetes.Interface
	CustomStorageAvailable      bool
	PageBrokerControlSocketPath string
	PageBrokerRestoreMode       string

	// Decided by the caller, so both gates reach the same answer.
	SkipCompatCheck bool
}

// RestoreResult identifies the processes a successful restore leaves behind.
type RestoreResult struct {
	// PlaceholderHostPID is the placeholder container's host PID, so callers
	// can reach into the container's mount namespace (e.g. to write sentinels
	// under /snapshot-control) without re-resolving via the runtime.
	PlaceholderHostPID int
	// RestoredPID is the restored process, relative to the container's PID
	// namespace.
	RestoredPID int
}

// Restore performs external restore for the given request.
// The DaemonSet side inspects the placeholder and launches nsrestore,
// which handles rootfs application, CRIU restore, and CUDA restore inside the namespace.
//
// A RestoreCleanupError still carries the result: the restore itself succeeded.
func Restore(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req RestoreRequest, mounts RestoreMounter) (restored RestoreResult, retErr error) {
	ctx = logr.NewContext(ctx, log)
	if mounts == nil {
		return RestoreResult{}, fmt.Errorf("restore mounter is required")
	}

	transactionID := ""
	broker := pagebroker.Client{ControlSocketPath: req.PageBrokerControlSocketPath}
	var gpu *pagebroker.CustomStorageExecution
	committed := false
	gpuRestoreComplete := false
	usePageBrokerGPU := false
	var terminateFailedRestore func(context.Context) error

	var cleanupErr error
	var activeMounts []restoreMount
	restoreComplete := false
	cleanup := func() {
		cleanupErr = errors.Join(cleanupErr, cleanupRestoreMounts(ctx, activeMounts, restoreComplete))
		activeMounts = nil
	}
	defer func() {
		if gpu != nil {
			defer gpu.Close()
		}
		if usePageBrokerGPU && transactionID != "" && !committed {
			var err error
			if gpu != nil && !gpuRestoreComplete {
				err = abortRestoreTransaction(ctx, gpu, terminateFailedRestore)
			} else {
				abortCtx, cancel := context.WithTimeout(context.Background(), pageBrokerAbortTimeout)
				err = broker.Abort(abortCtx, transactionID)
				cancel()
			}
			if err != nil {
				if retErr != nil {
					retErr = errors.Join(retErr, fmt.Errorf("abort PageBroker restore: %w", err))
				} else {
					retErr = NewRestoreCleanupError(errors.Join(cleanupErr, fmt.Errorf("release PageBroker restore: %w", err)))
				}
			}
		}
		cleanup()
		if !usePageBrokerGPU && transactionID != "" && !committed {
			abortCtx, cancel := context.WithTimeout(context.Background(), pageBrokerAbortTimeout)
			defer cancel()
			if err := broker.Abort(abortCtx, transactionID); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("abort PageBroker restore %q: %w", transactionID, err))
			}
		}
		if cleanupErr == nil {
			return
		}
		log.Error(cleanupErr, "restore cleanup failed")
		if retErr == nil {
			retErr = NewRestoreCleanupError(cleanupErr)
		}
	}()

	restoreStart := time.Now()
	log.Info("=== Starting external restore ===",
		"content_uid", req.ContentUID,
		"pod", req.PodName,
		"namespace", req.PodNamespace,
		"source_container", req.ArtifactContainerName,
		"destination_container", req.DestinationContainerName,
	)

	artifactPath, err := nsmount.ResolveArtifact(req.BasePath, req.ContentUID, req.ArtifactContainerName)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("resolve checkpoint artifact: %w", err)
	}
	manifest, err := types.ReadManifest(artifactPath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	if err := validateRestoreManifest(req, manifest); err != nil {
		return RestoreResult{}, err
	}
	if manifest.CuInterpose != nil {
		// These libraries belong to the checkpointed process. The agent supplies their
		// files, but an upgraded bundle may differ from the bytes loaded at capture.
		if err := cuda.VerifyCuInterposeLibraryIdentity(nsmount.CuInterposeBundlePath, manifest.CuInterpose); err != nil {
			return RestoreResult{}, err
		}
	}

	usePageBrokerGPU = manifest.CUDA.CustomStorage
	if err := validateCustomStorageRestore(req, manifest); err != nil {
		return RestoreResult{}, err
	}

	snap, gpuDeviceMapDuration, err := inspectRestore(ctx, rt, log, req, manifest)
	if err != nil {
		return RestoreResult{}, err
	}

	var gpuContext *pagebroker.GpuContext
	if usePageBrokerGPU {
		gpuContext, err = pagebroker.NewGPUContext(manifest.CUDA.PIDs, snap.TargetGPUUUIDs, snap.CUDADeviceMap)
		if err != nil {
			return RestoreResult{}, err
		}
	}

	bundleMount, err := mounts.MountBundle(ctx, snap.PlaceholderPID)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("mount agent bundle into placeholder: %w", err)
	}
	activeMounts = append(activeMounts, restoreMount{
		action: "unmount agent bundle from placeholder",
		point:  bundleMount,
	})

	if manifest.CuInterpose != nil {
		shimMount, err := mounts.MountCuInterpose(ctx, bundleMount)
		if err != nil {
			return RestoreResult{}, fmt.Errorf("mount cuinterpose into placeholder: %w", err)
		}
		activeMounts = append(activeMounts, restoreMount{
			action:        "unmount cuinterpose from placeholder",
			point:         shimMount,
			keepOnSuccess: true,
		})
	}

	containerCheckpointPath := nsmount.CheckpointDst
	var pageBrokerStageDuration, pageBrokerMountDuration, pageBrokerCommitDuration time.Duration
	transactionID = uuid.NewString()
	broker = pagebroker.Client{ControlSocketPath: req.PageBrokerControlSocketPath}
	stageStart := time.Now()
	direct := req.PageBrokerRestoreMode != "staged"
	var staged string
	if direct {
		err = broker.DirectRestore(ctx, transactionID, artifactPath)
	} else {
		staged, err = broker.StagedRestore(ctx, transactionID, artifactPath)
	}
	pageBrokerStageDuration = time.Since(stageStart)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("prepare PageBroker restore: %w", err)
	}
	if usePageBrokerGPU {
		gpu, err = broker.OpenCustomStorageExecution(transactionID, gpuContext)
		if err != nil {
			return RestoreResult{}, err
		}
	}
	mountStart := time.Now()
	var sourceMount nsmount.MountPoint
	if direct {
		sourceMount, err = mounts.MountArtifact(ctx, bundleMount, artifactPath)
	} else {
		sourceMount, err = mounts.MountPageBroker(ctx, bundleMount, staged)
		containerCheckpointPath = nsmount.PageBrokerDst
	}
	pageBrokerMountDuration = time.Since(mountStart)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("mount PageBroker restore source: %w", err)
	}
	activeMounts = append(activeMounts, restoreMount{
		action: "unmount PageBroker restore source from placeholder",
		point:  sourceMount,
	})

	result, err := execNSRestore(ctx, log, req, snap, bundleMount, containerCheckpointPath, gpu)
	if err != nil {
		if usePageBrokerGPU {
			// Abort may stop the placeholder before the controller can write its
			// failure marker. Preserve the one-restore-per-pod contract first.
			if markerErr := snapshotruntime.WriteControlSentinel(snap.PlaceholderPID, podcontract.RestoreFailedFile, []byte("restore failed\n")); markerErr != nil {
				log.Error(markerErr, "Failed to write restore-failed sentinel")
			}
			terminateFailedRestore = func(stopCtx context.Context) error {
				return rt.TerminateContainer(stopCtx, req.ContainerID)
			}
		}
		return RestoreResult{}, fmt.Errorf("nsrestore failed: %w", err)
	}
	gpuRestoreComplete = usePageBrokerGPU
	restoreSource := activeMounts[len(activeMounts)-1]
	if err := restoreSource.point.Unmount(ctx); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", restoreSource.action, err))
	}
	activeMounts = activeMounts[:len(activeMounts)-1]
	commitStart := time.Now()
	if err := broker.Commit(ctx, transactionID); err != nil {
		log.Error(err, "failed to commit PageBroker restore")
	} else {
		committed = true
	}
	pageBrokerCommitDuration = time.Since(commitStart)

	if result.CleanupError != nil {
		cleanupErr = errors.Join(cleanupErr, result.CleanupError)
	}
	if err := validateRestoredProcess(snap.TargetRoot, result.RestoredPID, log); err != nil {
		return RestoreResult{}, err
	}

	// Restored processes still need these paths for lazy core loading and
	// LD_PRELOAD in later child processes. Only temporary restore mounts go away.
	// Release closes the agent's fd. The retained bind belongs to the workload
	// mount namespace and disappears when that namespace is destroyed.
	restoreComplete = true
	// CustomStorage restores clean up in the deferred PageBroker release path.
	if !usePageBrokerGPU {
		cleanup()
	}
	wall := time.Since(restoreStart)
	unaccounted := remainingDuration(wall,
		pageBrokerStageDuration,
		pageBrokerMountDuration,
		pageBrokerCommitDuration,
		gpuDeviceMapDuration,
		result.OverlayCaptureDuration,
		result.CRIUPrepareDuration,
		result.CRIURestoreDuration,
		result.CUDARestoreDuration,
	)
	summary := map[string]any{
		"duration": wall.String(),
		"phases": map[string]string{
			"pagebroker_stage":  pageBrokerStageDuration.String(),
			"pagebroker_mount":  pageBrokerMountDuration.String(),
			"pagebroker_commit": pageBrokerCommitDuration.String(),
			"gpu_device_map":    gpuDeviceMapDuration.String(),
			"overlay_capture":   result.OverlayCaptureDuration.String(),
			"criu_prepare":      result.CRIUPrepareDuration.String(),
			"criu_restore":      result.CRIURestoreDuration.String(),
			"cuda_restore":      result.CUDARestoreDuration.String(),
			"unaccounted":       unaccounted.String(),
		},
	}
	if !req.StartedAt.IsZero() {
		summary["started_to_complete"] = time.Since(req.StartedAt).String()
	}
	log.Info("Restore timing summary", "restore", summary)
	log.Info("=== External restore completed ===",
		"restored_pid", result.RestoredPID,
		"placeholder_host_pid", snap.PlaceholderPID,
	)

	return RestoreResult{PlaceholderHostPID: snap.PlaceholderPID, RestoredPID: result.RestoredPID}, nil
}

func remainingDuration(wall time.Duration, parts ...time.Duration) time.Duration {
	var sum time.Duration
	for _, part := range parts {
		sum += part
	}
	if wall <= sum {
		return 0
	}
	return wall - sum
}

func validateRestoredProcess(targetRoot string, restoredPID int, log logr.Logger) error {
	procRoot := filepath.Join(targetRoot, "proc")
	if err := snapshotruntime.ValidateProcessState(procRoot, restoredPID); err != nil {
		restoreLogPath := filepath.Join(targetRoot, "var", "criu-work", criu.RestoreLogFilename)
		logging.LogProcessDiagnostics(procRoot, restoredPID, restoreLogPath, log)
		return fmt.Errorf("restored process failed post-restore validation: %w", err)
	}
	return nil
}

func validateRestoreManifest(req RestoreRequest, manifest *types.CheckpointManifest) error {
	if manifest.Artifact.ContentUID != req.ContentUID || manifest.Artifact.ContainerName != req.ArtifactContainerName {
		return fmt.Errorf(
			"checkpoint manifest artifact %s/%s does not match requested artifact %s/%s",
			manifest.Artifact.ContentUID,
			manifest.Artifact.ContainerName,
			req.ContentUID,
			req.ArtifactContainerName,
		)
	}
	return nil
}

func validateCustomStorageRestore(req RestoreRequest, manifest *types.CheckpointManifest) error {
	if !manifest.CUDA.CustomStorage {
		return nil
	}
	if !req.CustomStorageAvailable {
		return fmt.Errorf("PageBroker does not support CustomStorage restore")
	}
	return nil
}

func inspectRestore(
	ctx context.Context,
	rt snapshotruntime.Runtime,
	log logr.Logger,
	req RestoreRequest,
	manifest *types.CheckpointManifest,
) (*types.RestoreContainerSnapshot, time.Duration, error) {
	var (
		placeholderPID int
		ociSpec        *specs.Spec
		err            error
	)
	if req.ContainerID != "" {
		placeholderPID, ociSpec, err = rt.ResolveContainer(ctx, req.ContainerID)
	} else {
		placeholderPID, ociSpec, err = rt.ResolveContainerByPod(ctx, req.PodName, req.PodNamespace, req.DestinationContainerName)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("failed to resolve placeholder container: %w", err)
	}
	log.V(1).Info("Resolved placeholder container", "pid", placeholderPID)

	// Read only for the image-digest check, which treats a blank value as
	// unknown, so neither a skipped gate nor a runtime that cannot answer is
	// worth failing a restore over.
	targetImageID := ""
	if !req.SkipCompatCheck && manifest.K8s.ImageID != "" {
		if req.ContainerID == "" {
			log.Info("No container ID for this restore; not comparing the runtime image ID")
		} else {
			targetImageID, err = rt.ResolveContainerImageID(ctx, req.ContainerID)
			if err != nil {
				log.Error(err, "Failed to resolve the placeholder image ID; not comparing it",
					"containerID", req.ContainerID)
				targetImageID = ""
			}
		}
	}

	cgroupRoot, err := snapshotruntime.ResolveCgroupRootFromHostPID(placeholderPID)
	if err != nil {
		log.Error(err, "Failed to resolve placeholder cgroup root; proceeding without explicit cgroup remap")
		cgroupRoot = ""
	}

	targetRoot := fmt.Sprintf("%s/%d/root", snapshotruntime.HostProcPath, placeholderPID)

	var (
		targetGPUs       compat.GPUInfo
		targetGPUUUIDs   []string
		discoverDuration time.Duration
	)
	if !manifest.CUDA.IsEmpty() {
		if len(manifest.CUDA.SourceGPUUUIDs) == 0 {
			return nil, 0, fmt.Errorf("missing source GPU UUIDs in checkpoint manifest")
		}
		discoverStart := time.Now()
		var env []string
		if ociSpec != nil && ociSpec.Process != nil {
			env = ociSpec.Process.Env
		}
		targetGPUs, err = cuda.DiscoverGPUs(ctx, req.Clientset, req.PodName, req.PodNamespace,
			req.DestinationContainerName, snapshotruntime.HostProcPath, placeholderPID, env, log)
		discoverDuration = time.Since(discoverStart)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to get target GPU UUIDs: %w", err)
		}
		targetGPUUUIDs = cuda.GPUUUIDs(targetGPUs)
	}

	deviceMapStart := time.Now()
	cudaDeviceMap, gpuMountAliases, err := prepareGPUMapping(
		log, manifest, targetGPUUUIDs,
		func() (map[string]string, error) {
			return cuda.ResolveDevicePaths(snapshotruntime.HostProcPath, placeholderPID, targetGPUUUIDs)
		},
	)
	if err != nil {
		return nil, 0, err
	}
	deviceMapDuration := time.Since(deviceMapStart)

	if err := inspectCompatibility(log, manifest, targetGPUs, gpuMountAliases, targetRoot, targetImageID, req.SkipCompatCheck); err != nil {
		return nil, 0, err
	}
	// Even when policy checks are skipped, CUDA requires one target per source.
	if len(targetGPUUUIDs) != len(manifest.CUDA.SourceGPUUUIDs) {
		return nil, 0, fmt.Errorf("source and target GPU counts differ")
	}

	return &types.RestoreContainerSnapshot{
		TargetGPUUUIDs:  targetGPUUUIDs,
		PlaceholderPID:  placeholderPID,
		TargetRoot:      targetRoot,
		CgroupRoot:      cgroupRoot,
		CUDADeviceMap:   cudaDeviceMap,
		GPUMountAliases: gpuMountAliases,
	}, discoverDuration + deviceMapDuration, nil
}

// existingMountPaths reports which recorded mount destinations resolve inside
// the placeholder's rootfs. Only what the checkpoint recorded is looked up, so a
// gate on this path costs one stat per volume the checkpoint actually used.
//
// Only a path that is definitely absent is left out. Any other stat failure is
// this agent failing to look rather than the pod missing a volume, and reporting
// it as missing would refuse a restore that would have worked.
func existingMountPaths(targetRoot string, destinations []string, aliases map[string]string) []string {
	existing := make([]string, 0, len(destinations))
	for _, destination := range destinations {
		path := destination
		if alias, ok := aliases[path]; ok {
			path = alias
		}
		if _, err := os.Stat(filepath.Join(targetRoot, path)); !os.IsNotExist(err) {
			existing = append(existing, destination)
		}
	}
	return existing
}

func execNSRestore(ctx context.Context, log logr.Logger, req RestoreRequest, snap *types.RestoreContainerSnapshot, mp nsmount.MountPoint, checkpointPath string, gpu *pagebroker.CustomStorageExecution) (*RestoreInNamespaceResult, error) {

	cmd, closeFiles, err := snapshotruntime.CommandInNamespaces(ctx, snap.PlaceholderPID, mp.NsFd(),
		snap.TargetRoot, filepath.Join(nsmount.SnapshotBinSrc, "nsrestore"))
	if err != nil {
		return nil, err
	}
	defer closeFiles()
	args := []string{"--checkpoint-path", checkpointPath, "--bundle-dir", nsmount.SnapshotBinDst}

	if snap.CUDADeviceMap != "" {
		args = append(args, "--cuda-device-map", snap.CUDADeviceMap)
	}
	if len(snap.GPUMountAliases) > 0 {
		paths, err := json.Marshal(snap.GPUMountAliases)
		if err != nil {
			return nil, err
		}
		args = append(args, "--gpu-mount-aliases", string(paths))
	}
	if snap.CgroupRoot != "" {
		args = append(args, "--cgroup-root", snap.CgroupRoot)
	}
	if req.TargetPodIP != "" {
		args = append(args, "--target-pod-ip", req.TargetPodIP)
	}

	cmd.Args = append(cmd.Args, args...)
	if gpu != nil {
		hostProc, err := os.Open(snapshotruntime.HostProcPath)
		if err != nil {
			return nil, fmt.Errorf("open host proc for GPU PID resolution: %w", err)
		}
		defer hostProc.Close()
		closeGPUFiles, err := addCustomStorageFiles(ctx, cmd, gpu, hostProc)
		if err != nil {
			return nil, err
		}
		defer closeGPUFiles()
		// addCustomStorageFiles replaces process-group SIGKILL with cooperative
		// cancellation. Wait for nsrestore to drain GPU work instead of abandoning
		// it after the default wait delay.
		cmd.WaitDelay = 0
	}
	log.V(1).Info("Executing nsenter + nsrestore", "cmd", cmd.String())

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("nsrestore failed: %w\nstdout: %s", err, stdout.String())
	}

	var result RestoreInNamespaceResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("failed to parse nsrestore result: %w\nstdout: %s", err, stdout.String())
	}
	if result.RestoredPID <= 0 {
		return nil, fmt.Errorf("nsrestore returned invalid PID %d", result.RestoredPID)
	}

	return &result, nil
}

const firstExtraFileDescriptor = 3

func addCustomStorageFiles(ctx context.Context, cmd *exec.Cmd, execution *pagebroker.CustomStorageExecution, hostProc *os.File) (func(), error) {
	gpuContext, err := json.Marshal(execution.GPUContext)
	if err != nil {
		return nil, fmt.Errorf("encode GPU context: %w", err)
	}
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create nsrestore cancellation pipe: %w", err)
	}
	// nsenter forks for PID namespace entry. Keep cancellation active even if
	// its wrapper exits while nsrestore still holds the stdout pipe open.
	stopCancel := context.AfterFunc(ctx, func() { _ = cancelWrite.Close() })
	cmd.Cancel = func() error {
		_ = cancelWrite.Close()
		return nil
	}
	socketDirectoryFD := firstExtraFileDescriptor + len(cmd.ExtraFiles)
	hostProcFD := socketDirectoryFD + 1
	socketFD := socketDirectoryFD + 2
	cancelFD := socketDirectoryFD + 3
	cmd.Args = append(cmd.Args,
		"--pagebroker-transaction", execution.TransactionID,
		"--pagebroker-socket-directory-fd", strconv.Itoa(socketDirectoryFD),
		"--host-proc-fd", strconv.Itoa(hostProcFD),
		"--pagebroker-execution-fd", strconv.Itoa(socketFD),
		"--cancel-fd", strconv.Itoa(cancelFD),
		"--pagebroker-socket-name", execution.SocketName,
		"--gpu-context", string(gpuContext),
	)
	cmd.ExtraFiles = append(cmd.ExtraFiles, execution.SocketDirectory, hostProc, execution.Socket, cancelRead)
	return func() {
		stopCancel()
		cancelRead.Close()
		cancelWrite.Close()
	}, nil
}

// abortRestoreTransaction keeps failed targets alive until the broker confirms
// that all imported CUDA mappings and storage transfers have been drained.
func abortRestoreTransaction(ctx context.Context, gpu *pagebroker.CustomStorageExecution, terminate func(context.Context) error) error {
	abortErr := gpu.Abort(ctx)
	if terminate != nil {
		stopCtx, stop := context.WithTimeout(context.Background(), failedRestoreTerminationTimeout)
		defer stop()
		if err := terminate(stopCtx); err != nil {
			return errors.Join(abortErr, fmt.Errorf("terminate failed restore after GPU drain: %w", err))
		}
	}
	return abortErr
}
