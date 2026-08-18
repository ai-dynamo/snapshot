// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// Mounter mounts src at dst inside a placeholder container's mount namespace.
type Mounter interface {
	Mount(ctx context.Context, pid int, src, dst string) (nsmount.MountPoint, error)
}

// Mounters carries the executable bundle and non-executable artifact policies.
type Mounters struct {
	Bundle   Mounter
	Artifact Mounter
}

// RestoreCleanupError reports that restore work completed but a required
// namespace mount could not be removed. The controller treats it as fatal.
type RestoreCleanupError struct {
	Action string
	Err    error
}

func (e *RestoreCleanupError) Error() string { return fmt.Sprintf("%s: %v", e.Action, e.Err) }
func (e *RestoreCleanupError) Unwrap() error { return e.Err }

// RestoreRequest holds the parameters for a restore operation.
type RestoreRequest struct {
	CheckpointID    string
	ArtifactVersion string
	BasePath        string
	ContainerID     string
	StartedAt       time.Time
	PodName         string
	PodNamespace    string
	TargetPodIP     string
	ContainerName   string
	Clientset       kubernetes.Interface
}

// Restore performs external restore for the given request.
// Returns the namespace-relative PID of the restored process.
// The DaemonSet side inspects the placeholder and launches nsrestore,
// which handles rootfs application, CRIU restore, and CUDA restore inside the namespace.
//
// Returns the placeholder container's host PID so callers can reach into the
// container's mount namespace (e.g. to write sentinels under /snapshot-control)
// without re-resolving via the runtime.
func Restore(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req RestoreRequest, mounts Mounters) (placeholderPID int, retErr error) {
	restoreStart := time.Now()
	log.Info("=== Starting external restore ===",
		"checkpoint_id", req.CheckpointID,
		"pod", req.PodName,
		"namespace", req.PodNamespace,
		"container", req.ContainerName,
	)

	artifactPath, err := nsmount.ResolveArtifact(req.BasePath, req.CheckpointID, req.ArtifactVersion)
	if err != nil {
		return 0, fmt.Errorf("resolve checkpoint artifact: %w", err)
	}
	manifest, err := types.ReadManifest(artifactPath)
	if err != nil {
		return 0, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	if manifest.CheckpointID != req.CheckpointID {
		return 0, fmt.Errorf("checkpoint manifest ID %q does not match requested ID %q", manifest.CheckpointID, req.CheckpointID)
	}

	// Phase 1: Host inspect — resolve placeholder, discover target GPUs, build device map.
	hostInspectStart := time.Now()
	snap, err := inspectRestore(ctx, rt, log, req, manifest)
	if err != nil {
		return 0, err
	}
	hostInspectDuration := time.Since(hostInspectStart)

	// Phase 2: Mount agent binaries and the checkpoint artifact. Deferred
	// cleanup unwinds in reverse order: artifact first, then bundle.
	injectStart := time.Now()
	bundleMount, err := mounts.Bundle.Mount(ctx, snap.PlaceholderPID, nsmount.SnapshotBinSrc, nsmount.SnapshotBinDst)
	if err != nil {
		return 0, fmt.Errorf("mount agent bundle into placeholder: %w", err)
	}
	defer func() {
		if cleanupErr := bundleMount.Unmount(); cleanupErr != nil {
			log.Error(cleanupErr, "failed to clean bundle mount from placeholder namespace")
			setCleanupErrorIfSuccessful(&retErr, "unmount agent bundle from placeholder", cleanupErr)
		}
	}()
	artifactMount, err := mounts.Artifact.Mount(ctx, snap.PlaceholderPID, artifactPath, nsmount.CheckpointDst)
	if err != nil {
		return 0, fmt.Errorf("mount checkpoint artifact into placeholder: %w", err)
	}
	defer func() {
		if cleanupErr := artifactMount.Unmount(); cleanupErr != nil {
			log.Error(cleanupErr, "failed to clean artifact mount from placeholder namespace")
			setCleanupErrorIfSuccessful(&retErr, "unmount checkpoint artifact from placeholder", cleanupErr)
		}
	}()
	injectDuration := time.Since(injectStart)

	// Phase 3: Execute — nsrestore handles rootfs, CRIU restore, and CUDA restore inside namespace.
	result, err := execNSRestore(ctx, log, req, snap, bundleMount, nsmount.CheckpointDst)
	if err != nil {
		return 0, fmt.Errorf("nsrestore failed: %w", err)
	}
	restoreDuration := hostInspectDuration + injectDuration + result.TotalDuration()
	log.Info("Restore timing summary",
		"restore", map[string]any{
			"duration": restoreDuration.String(),
			"phases": map[string]string{
				"host_inspect_duration":    hostInspectDuration.String(),
				"inject_duration":          injectDuration.String(),
				"nsrestore_setup_duration": result.NSRestoreSetupDuration.String(),
				"criu_restore_duration":    result.CRIURestoreDuration.String(),
				"cuda_duration":            result.CUDADuration.String(),
			},
		},
	)
	if !req.StartedAt.IsZero() {
		log.Info("Restore wall time from agent detection",
			"started_to_restore_complete", time.Since(req.StartedAt),
		)
	}

	validationStart := time.Now()
	if err := validateRestoredProcess(snap.TargetRoot, result.RestoredPID, log); err != nil {
		return 0, err
	}

	log.Info("=== External restore completed ===",
		"restored_pid", result.RestoredPID,
		"placeholder_host_pid", snap.PlaceholderPID,
		"validation_duration", time.Since(validationStart),
		"total_duration", time.Since(restoreStart),
	)

	return snap.PlaceholderPID, nil
}

func setCleanupErrorIfSuccessful(retErr *error, action string, cleanupErr error) {
	// Namespace mount cleanup is part of restore correctness. Returning this
	// failure makes the controller mark the restore failed and kill the workload.
	if *retErr == nil {
		*retErr = &RestoreCleanupError{Action: action, Err: cleanupErr}
	}
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

func inspectRestore(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req RestoreRequest, m *types.CheckpointManifest) (*types.RestoreContainerSnapshot, error) {
	containerName := req.ContainerName
	if containerName == "" {
		containerName = "main"
	}

	var (
		placeholderPID int
		err            error
	)
	if req.ContainerID != "" {
		placeholderPID, _, err = rt.ResolveContainer(ctx, req.ContainerID)
	} else {
		placeholderPID, _, err = rt.ResolveContainerByPod(ctx, req.PodName, req.PodNamespace, containerName)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to resolve placeholder container: %w", err)
	}
	log.V(1).Info("Resolved placeholder container", "pid", placeholderPID)

	cgroupRoot, err := snapshotruntime.ResolveCgroupRootFromHostPID(placeholderPID)
	if err != nil {
		log.Error(err, "Failed to resolve placeholder cgroup root; proceeding without explicit cgroup remap")
		cgroupRoot = ""
	}

	cudaDeviceMap := ""
	if !m.CUDA.IsEmpty() {
		if len(m.CUDA.SourceGPUUUIDs) == 0 {
			return nil, fmt.Errorf("missing source GPU UUIDs in checkpoint manifest")
		}
		targetGPUUUIDs, err := cuda.DiscoverGPUUUIDs(
			ctx,
			req.Clientset,
			req.PodName,
			req.PodNamespace,
			containerName,
			snapshotruntime.HostProcPath,
			placeholderPID,
			log,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to get target GPU UUIDs: %w", err)
		}
		if len(targetGPUUUIDs) == 0 {
			return nil, fmt.Errorf("missing target GPU UUIDs for %s/%s container %s", req.PodNamespace, req.PodName, containerName)
		}
		cudaDeviceMap, err = cuda.BuildDeviceMap(m.CUDA.SourceGPUUUIDs, targetGPUUUIDs, log)
		if err != nil {
			return nil, fmt.Errorf("failed to build CUDA device map: %w", err)
		}
		log.V(1).Info("GPU UUIDs for device map",
			"source_uuids", m.CUDA.SourceGPUUUIDs,
			"target_uuids", targetGPUUUIDs,
			"device_map", cudaDeviceMap,
		)
	}

	return &types.RestoreContainerSnapshot{
		PlaceholderPID: placeholderPID,
		TargetRoot:     fmt.Sprintf("%s/%d/root", snapshotruntime.HostProcPath, placeholderPID),
		CgroupRoot:     cgroupRoot,
		CUDADeviceMap:  cudaDeviceMap,
	}, nil
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
