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
	"k8s.io/client-go/kubernetes"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/logging"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
)

// RestoreMounter installs the fixed binary bundle and one validated checkpoint
// artifact inside a placeholder container's mount namespace.
type RestoreMounter interface {
	MountBundle(ctx context.Context, pid int) (nsmount.MountPoint, error)
	MountArtifact(ctx context.Context, namespaceMount nsmount.MountPoint, artifactPath string) (nsmount.MountPoint, error)
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
	action string
	point  nsmount.MountPoint
}

func cleanupRestoreMounts(ctx context.Context, mounts []restoreMount) error {
	var cleanupErr error
	cleanupCtx := context.WithoutCancel(ctx)
	for i := len(mounts) - 1; i >= 0; i-- {
		if err := mounts[i].point.Unmount(cleanupCtx); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", mounts[i].action, err))
		}
	}
	return cleanupErr
}

// RestoreRequest holds the parameters for a restore operation.
type RestoreRequest struct {
	ContentUID               string
	BasePath                 string
	ContainerID              string
	StartedAt                time.Time
	PodName                  string
	PodNamespace             string
	TargetPodIP              string
	ArtifactContainerName    string
	DestinationContainerName string
	Clientset                kubernetes.Interface

	// SkipCompatCheck carries the decision the caller already made, so the
	// second gate cannot reach a different answer than the first: one restore
	// is either checked or it is not.
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

	var cleanupErr error
	var activeMounts []restoreMount
	cleanup := func() {
		cleanupErr = errors.Join(cleanupErr, cleanupRestoreMounts(ctx, activeMounts))
		activeMounts = nil
	}
	defer func() {
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
	if err := validateRestoreManifest(req, manifest); err != nil {
		return 0, err
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

	artifactMount, err := mounts.MountArtifact(ctx, bundleMount, artifactPath)
	if err != nil {
		return 0, fmt.Errorf("mount checkpoint artifact into placeholder: %w", err)
	}
	activeMounts = append(activeMounts, restoreMount{
		action: "unmount checkpoint artifact from placeholder",
		point:  artifactMount,
	})

	result, err := execNSRestore(ctx, log, req, snap, bundleMount, nsmount.CheckpointDst)
	if err != nil {
		return 0, fmt.Errorf("nsrestore failed: %w", err)
	}
	if result.CleanupError != nil {
		cleanupErr = errors.Join(cleanupErr, result.CleanupError)
	}
	if err := validateRestoredProcess(snap.TargetRoot, result.RestoredPID, log); err != nil {
		return 0, err
	}

	cleanup()
	wall := time.Since(restoreStart)
	unaccounted := remainingDuration(wall,
		gpuDeviceMapDuration,
		result.OverlayCaptureDuration,
		result.CRIUPrepareDuration,
		result.CRIURestoreDuration,
		result.CUDARestoreDuration,
	)
	summary := map[string]any{
		"duration": wall.String(),
		"phases": map[string]string{
			"gpu_device_map":  gpuDeviceMapDuration.String(),
			"overlay_capture": result.OverlayCaptureDuration.String(),
			"criu_prepare":    result.CRIUPrepareDuration.String(),
			"criu_restore":    result.CRIURestoreDuration.String(),
			"cuda_restore":    result.CUDARestoreDuration.String(),
			"unaccounted":     unaccounted.String(),
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

func inspectRestore(
	ctx context.Context,
	rt snapshotruntime.Runtime,
	log logr.Logger,
	req RestoreRequest,
	manifest *types.CheckpointManifest,
) (*types.RestoreContainerSnapshot, time.Duration, error) {
	var (
		placeholderPID int
		err            error
	)
	if req.ContainerID != "" {
		placeholderPID, _, err = rt.ResolveContainer(ctx, req.ContainerID)
	} else {
		placeholderPID, _, err = rt.ResolveContainerByPod(ctx, req.PodName, req.PodNamespace, req.DestinationContainerName)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("failed to resolve placeholder container: %w", err)
	}
	log.V(1).Info("Resolved placeholder container", "pid", placeholderPID)

	targetImageID := ""
	if manifest.K8s.ImageID != "" {
		if req.ContainerID == "" {
			return nil, 0, fmt.Errorf("container ID is required to compare the runtime image ID")
		}
		targetImageID, err = rt.ResolveContainerImageID(ctx, req.ContainerID)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to resolve placeholder image ID: %w", err)
		}
	}

	cgroupRoot, err := snapshotruntime.ResolveCgroupRootFromHostPID(placeholderPID)
	if err != nil {
		log.Error(err, "Failed to resolve placeholder cgroup root; proceeding without explicit cgroup remap")
		cgroupRoot = ""
	}

	targetRoot := fmt.Sprintf("%s/%d/root", snapshotruntime.HostProcPath, placeholderPID)

	var (
		targetGPUs        compat.GPUFacts
		targetGPUUUIDs    []string
		discoverDuration  time.Duration
		deviceMapDuration time.Duration
	)
	if !manifest.CUDA.IsEmpty() {
		if len(manifest.CUDA.SourceGPUUUIDs) == 0 {
			return nil, 0, fmt.Errorf("missing source GPU UUIDs in checkpoint manifest")
		}
		discoverStart := time.Now()
		targetGPUs, err = cuda.DiscoverGPUFacts(
			ctx,
			req.Clientset,
			req.PodName,
			req.PodNamespace,
			req.DestinationContainerName,
			snapshotruntime.HostProcPath,
			placeholderPID,
			log,
		)
		discoverDuration = time.Since(discoverStart)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to get target GPU UUIDs: %w", err)
		}
		for _, device := range targetGPUs.Devices {
			targetGPUUUIDs = append(targetGPUUUIDs, device.UUID)
		}
		if len(targetGPUUUIDs) == 0 {
			return nil, 0, fmt.Errorf("missing target GPU UUIDs for %s/%s container %s", req.PodNamespace, req.PodName, req.DestinationContainerName)
		}
	}

	// Gate B, once the placeholder is resolved and this node's own facts are
	// readable. It runs ahead of BuildDeviceMap, whose positional pairing turns
	// a GPU difference into a device-map error that names neither GPU.
	if err := inspectCompatibility(log, manifest, targetGPUs, targetRoot, targetImageID, req.SkipCompatCheck); err != nil {
		return nil, 0, err
	}

	cudaDeviceMap := ""
	if len(targetGPUUUIDs) > 0 {
		deviceMapStart := time.Now()
		cudaDeviceMap, err = cuda.BuildDeviceMap(manifest.CUDA.SourceGPUUUIDs, targetGPUUUIDs, log)
		deviceMapDuration = time.Since(deviceMapStart)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to build CUDA device map: %w", err)
		}
		log.V(1).Info("GPU UUIDs for device map",
			"source_uuids", manifest.CUDA.SourceGPUUUIDs,
			"target_uuids", targetGPUUUIDs,
			"device_map", cudaDeviceMap,
		)
	}

	return &types.RestoreContainerSnapshot{
		PlaceholderPID: placeholderPID,
		TargetRoot:     targetRoot,
		CgroupRoot:     cgroupRoot,
		CUDADeviceMap:  cudaDeviceMap,
	}, discoverDuration + deviceMapDuration, nil
}

// existingMounts reports which of the recorded destinations resolve inside the
// placeholder's rootfs. Only what the checkpoint recorded is looked up, so a gate
// on this path costs one stat per volume the checkpoint actually used.
//
// Only a path that is definitely absent is left out. Any other stat failure is
// this agent failing to look rather than the pod missing a volume, and reporting
// it as missing would refuse a restore that would have worked.
func existingMounts(targetRoot string, destinations []string) []string {
	existing := make([]string, 0, len(destinations))
	for _, destination := range destinations {
		if _, err := os.Stat(filepath.Join(targetRoot, destination)); !os.IsNotExist(err) {
			existing = append(existing, destination)
		}
	}
	return existing
}

// execNSRestore launches the nsrestore binary inside the placeholder container's
// namespaces via nsenter and parses the restored PID from stdout JSON.
//
// Security hardening in place:
//
//  1. Mount-namespace pinning: mp.NsFd() is the /proc/<pid>/ns/mnt fd opened at
//     mount time. Passing it via --mount=/proc/self/fd/N to nsenter pins the mount
//     namespace against PID reuse. The remaining four namespaces (uts, ipc, net,
//     pid) are still resolved via -t <pid> and are not protected against reuse.
//
//  2. nsrestore binary fd: we open nsrestore from the agent host side (SnapshotBinSrc)
//     before entering any namespace and exec it via /proc/self/fd/N. This protects
//     the nsrestore binary itself against path-based substitution inside the
//     container. Binaries that nsrestore subsequently loads (criu, ip, tar, .so
//     files) are still resolved by PATH/LD_LIBRARY_PATH inside the container's
//     mount namespace.
func execNSRestore(ctx context.Context, log logr.Logger, req RestoreRequest, snap *types.RestoreContainerSnapshot, mp nsmount.MountPoint, checkpointPath string) (*RestoreInNamespaceResult, error) {

	// Open nsrestore from the agent host side before entering the container
	// namespace, so the binary fd is immune to rename attacks inside the container.
	binaryFile, err := os.Open(filepath.Join(nsmount.SnapshotBinSrc, "nsrestore"))
	if err != nil {
		return nil, fmt.Errorf("open nsrestore from agent bundle: %w", err)
	}
	defer binaryFile.Close()

	// ExtraFiles[0] → child fd 3, ExtraFiles[1] → child fd 4.
	// These constants mirror nsFdChildNum in mount.go (ExtraFiles[0] = fd 3).
	const (
		nsFdChild     = 3 // mp.NsFd() passed as ExtraFiles[0]
		binaryFdChild = 4 // binaryFile passed as ExtraFiles[1]
	)

	bundleDir := nsmount.SnapshotBinDst // bundle root as seen inside the container
	var args []string

	nsFd := mp.NsFd()
	if nsFd != nil {
		// Use the pinned ns fd for the mount namespace; keep -t for the other
		// namespaces (user, ipc, net, pid). This decouples mount-ns entry from
		// PID liveness.
		args = []string{
			fmt.Sprintf("--mount=/proc/self/fd/%d", nsFdChild),
			"-t", strconv.Itoa(snap.PlaceholderPID),
			// Intentionally exclude cgroup namespace (-C): CRIU must manage cgroups
			// from the host-visible hierarchy so --cgroup-root remap works.
			"-u", "-i", "-n", "-p",
			"--", fmt.Sprintf("/proc/self/fd/%d", binaryFdChild),
		}
	} else {
		return nil, fmt.Errorf("execNSRestore: mp.NsFd() is nil; mount point was not properly initialized")
	}
	args = append(args,
		"--checkpoint-path", checkpointPath,
		"--bundle-dir", bundleDir,
	)
	if snap.CUDADeviceMap != "" {
		args = append(args, "--cuda-device-map", snap.CUDADeviceMap)
	}
	if snap.CgroupRoot != "" {
		args = append(args, "--cgroup-root", snap.CgroupRoot)
	}
	if req.TargetPodIP != "" {
		args = append(args, "--target-pod-ip", req.TargetPodIP)
	}

	cmd := exec.CommandContext(ctx, "nsenter", args...)
	// Inherit the agent environment so nsrestore uses the same logger settings.
	cmd.Env = os.Environ()
	cmd.ExtraFiles = []*os.File{nsFd, binaryFile}
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
