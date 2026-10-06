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
	"path/filepath"
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
)

// RestoreMounter installs the fixed binary bundle and one validated checkpoint
// artifact inside a placeholder container's mount namespace.
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
	PageBrokerRequested         bool
	PageBrokerEnabled           bool
	PageBrokerControlSocketPath string
	PageBrokerRestoreMode       string

	// Decided by the caller, so both gates reach the same answer.
	SkipCompatCheck bool
}

// Restore performs external restore for the given request.
// Returns the namespace-relative PID of the restored process.
// The DaemonSet side inspects the placeholder and launches nsrestore,
// which handles rootfs application, CRIU restore, and CUDA restore inside the namespace.
//
// Returns the placeholder container's host PID so callers can reach into the
// container's mount namespace (e.g. to write sentinels under /snapshot-control)
// without re-resolving via the runtime.
func Restore(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req RestoreRequest, mounts RestoreMounter) (placeholderPID int, retErr error) {
	if mounts == nil {
		return 0, fmt.Errorf("restore mounter is required")
	}

	brokered := req.PageBrokerRequested && req.PageBrokerEnabled
	transactionID := ""
	var broker pagebroker.Client
	committed := false
	useGPU := false
	var terminateFailedRestore func(context.Context) error
	defer func() {
		if !useGPU && transactionID != "" && !committed {
			abortCtx, cancel := context.WithTimeout(context.Background(), pageBrokerAbortTimeout)
			defer cancel()
			_ = broker.Abort(abortCtx, transactionID)
		}
	}()

	var cleanupErr error
	var activeMounts []restoreMount
	restored := false
	cleanup := func() {
		cleanupErr = errors.Join(cleanupErr, cleanupRestoreMounts(ctx, activeMounts, restored))
		activeMounts = nil
	}
	defer func() {
		if useGPU && transactionID != "" && !committed {
			drained, err := abortRestoreTransaction(broker, transactionID, terminateFailedRestore)
			if err != nil {
				if retErr != nil {
					retErr = errors.Join(retErr, fmt.Errorf("abort PageBroker restore: %w", err))
				} else {
					retErr = NewRestoreCleanupError(errors.Join(cleanupErr, fmt.Errorf("release PageBroker restore: %w", err)))
				}
				// PageBroker has not confirmed GPU cleanup. Keep the mounts
				// because GPU work may still use these files.
				if !drained {
					if terminateFailedRestore != nil {
						retErr = &GPUDrainError{Err: retErr}
					}
					return
				}
			}
		}
		cleanup()
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
		return 0, fmt.Errorf("resolve checkpoint artifact: %w", err)
	}
	manifest, err := types.ReadManifest(artifactPath)
	if err != nil {
		return 0, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	if err := validateRestoreManifest(req, manifest, nsmount.CuInterposeBundlePath); err != nil {
		return 0, err
	}

	useGPU = manifest.CUDA.CustomStorage
	if useGPU {
		if !req.PageBrokerEnabled {
			return 0, fmt.Errorf("CustomStorage checkpoint requires PageBroker")
		}
		if !req.PageBrokerRequested {
			return 0, fmt.Errorf("CustomStorage restore requires nvidia.com/snapshot-pagebroker=true")
		}
		available, err := selectGPUCheckpoint(ctx, types.PageBrokerSpec{Enabled: true, ControlSocketPath: req.PageBrokerControlSocketPath}, true)
		if err != nil {
			return 0, err
		}
		if !available {
			return 0, fmt.Errorf("PageBroker cannot restore CustomStorage on this driver")
		}
	}

	snap, gpuDeviceMapDuration, err := inspectRestore(ctx, rt, log, req, manifest)
	if err != nil {
		return 0, err
	}

	bundleMount, err := mounts.MountBundle(ctx, snap.PlaceholderPID)
	if err != nil {
		return 0, fmt.Errorf("mount agent bundle into placeholder: %w", err)
	}
	activeMounts = append(activeMounts, restoreMount{
		action: "unmount agent bundle from placeholder",
		point:  bundleMount,
	})

	if manifest.CuInterpose != nil {
		shimMount, err := mounts.MountCuInterpose(ctx, bundleMount)
		if err != nil {
			return 0, fmt.Errorf("mount cuinterpose into placeholder: %w", err)
		}
		activeMounts = append(activeMounts, restoreMount{
			action:        "unmount cuinterpose from placeholder",
			point:         shimMount,
			keepOnSuccess: true,
		})
	}

	var gpu *pagebroker.GPUExecution
	containerCheckpointPath := nsmount.CheckpointDst
	var pageBrokerStageDuration, pageBrokerMountDuration, pageBrokerCommitDuration time.Duration
	if brokered {
		broker = pagebroker.Client{ControlSocketPath: req.PageBrokerControlSocketPath}
		transactionID = uuid.NewString()
		stageStart := time.Now()
		direct := req.PageBrokerRestoreMode == "direct"
		var staged string
		if direct {
			err = broker.DirectRestore(ctx, transactionID, artifactPath)
		} else {
			staged, err = broker.StagedRestore(ctx, transactionID, artifactPath)
		}
		pageBrokerStageDuration = time.Since(stageStart)
		if err != nil {
			return 0, fmt.Errorf("prepare PageBroker restore: %w", err)
		}
		if useGPU {
			context, err := gpuContext(manifest.CUDA.PIDs, snap.TargetGPUUUIDs, snap.CUDADeviceMap)
			if err != nil {
				return 0, err
			}
			gpu, err = broker.OpenGPUExecution(transactionID, context)
			if err != nil {
				return 0, err
			}
			defer gpu.Close()
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
			return 0, fmt.Errorf("mount PageBroker restore source: %w", err)
		}
		activeMounts = append(activeMounts, restoreMount{
			action: "unmount PageBroker restore source from placeholder",
			point:  sourceMount,
		})
	} else {
		artifactMount, err := mounts.MountArtifact(ctx, bundleMount, artifactPath)
		if err != nil {
			return 0, fmt.Errorf("mount checkpoint artifact into placeholder: %w", err)
		}
		activeMounts = append(activeMounts, restoreMount{
			action: "unmount checkpoint artifact from placeholder",
			point:  artifactMount,
		})
	}

	result, err := execNSRestore(ctx, log, req, snap, bundleMount, containerCheckpointPath, gpu)
	if err != nil {
		if useGPU {
			terminateFailedRestore = func(stopCtx context.Context) error {
				return rt.TerminateContainer(stopCtx, req.ContainerID)
			}
		}
		return 0, fmt.Errorf("nsrestore failed: %w", err)
	}
	if brokered {
		sourceMount := activeMounts[len(activeMounts)-1]
		if err := sourceMount.point.Unmount(ctx); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", sourceMount.action, err))
		}
		activeMounts = activeMounts[:len(activeMounts)-1]
	}
	if transactionID != "" {
		commitStart := time.Now()
		if err := broker.Commit(ctx, transactionID); err != nil {
			log.Error(err, "failed to commit PageBroker restore")
		} else {
			committed = true
		}
		pageBrokerCommitDuration = time.Since(commitStart)
	}
	if result.CleanupError != nil {
		cleanupErr = errors.Join(cleanupErr, result.CleanupError)
	}
	if err := validateRestoredProcess(snap.TargetRoot, result.RestoredPID, log); err != nil {
		return 0, err
	}

	// Restored processes still need these paths for lazy core loading and
	// LD_PRELOAD in later child processes. Only temporary restore mounts go away.
	// Release closes the agent's fd. The retained bind belongs to the workload
	// mount namespace and disappears when that namespace is destroyed.
	restored = true
	cleanup()
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

	return snap.PlaceholderPID, nil
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

func validateRestoreManifest(req RestoreRequest, manifest *types.CheckpointManifest, bundleDir string) error {
	if manifest.Artifact.ContentUID != req.ContentUID || manifest.Artifact.ContainerName != req.ArtifactContainerName {
		return fmt.Errorf(
			"checkpoint manifest artifact %s/%s does not match requested artifact %s/%s",
			manifest.Artifact.ContentUID,
			manifest.Artifact.ContainerName,
			req.ContentUID,
			req.ArtifactContainerName,
		)
	}
	// Executable identity is required even when compatibility policy is skipped.
	return cuda.CheckCuInterposeLibraries(bundleDir, manifest.CuInterpose)
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
		for _, device := range targetGPUs.Devices {
			targetGPUUUIDs = append(targetGPUUUIDs, device.UUID)
		}
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

func execNSRestore(ctx context.Context, log logr.Logger, req RestoreRequest, snap *types.RestoreContainerSnapshot, mp nsmount.MountPoint, checkpointPath string, gpu *pagebroker.GPUExecution) (*RestoreInNamespaceResult, error) {

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
		gpuContext, err := json.Marshal(gpu.Context)
		if err != nil {
			return nil, err
		}
		cmd.Args = append(cmd.Args, "--gpu-transaction", gpu.TransactionID,
			"--gpu-directory-fd", strconv.Itoa(3+len(cmd.ExtraFiles)),
			"--host-proc-fd", strconv.Itoa(4+len(cmd.ExtraFiles)),
			"--gpu-socket-name", gpu.SocketName, "--gpu-context", string(gpuContext))
		cmd.ExtraFiles = append(cmd.ExtraFiles, gpu.Directory, hostProc)
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

// abortRestoreTransaction keeps failed targets alive until the broker confirms
// that all imported CUDA mappings and storage transfers have been drained.
func abortRestoreTransaction(broker pagebroker.Client, transactionID string, terminate func(context.Context) error) (drained bool, err error) {
	abortCtx, cancel := context.WithTimeout(context.Background(), pageBrokerAbortTimeout)
	defer cancel()
	if err := broker.Abort(abortCtx, transactionID); err != nil {
		return false, err
	}
	if terminate != nil {
		stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := terminate(stopCtx); err != nil {
			return true, fmt.Errorf("terminate failed restore after GPU drain: %w", err)
		}
	}
	return true, nil
}
