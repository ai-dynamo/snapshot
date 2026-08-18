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
	"strings"
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

// Mounter mounts the agent binary bundle into a placeholder container's mount namespace.
type Mounter interface {
	Mount(ctx context.Context, pid int) (nsmount.MountPoint, error)
}

// RestoreRequest holds the parameters for a restore operation.
type RestoreRequest struct {
	CheckpointID                string
	CheckpointLocation          string
	ContainerCheckpointLocation string
	ContainerID                 string
	StartedAt                   time.Time
	PodName                     string
	PodNamespace                string
	TargetPodIP                 string
	ContainerName               string
	Clientset                   kubernetes.Interface
}

// Restore performs external restore for the given request.
// Returns the namespace-relative PID of the restored process.
// The DaemonSet side inspects the placeholder and launches nsrestore,
// which handles rootfs application, CRIU restore, and CUDA restore inside the namespace.
//
// Returns the placeholder container's host PID so callers can reach into the
// container's mount namespace (e.g. to write sentinels under /snapshot-control)
// without re-resolving via the runtime.
func Restore(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req RestoreRequest, mounter Mounter) (placeholderPID int, retErr error) {
	restoreStart := time.Now()
	log.Info("=== Starting external restore ===",
		"checkpoint_id", req.CheckpointID,
		"pod", req.PodName,
		"namespace", req.PodNamespace,
		"container", req.ContainerName,
	)

	snap, gpuDeviceMapDuration, err := inspectRestore(ctx, rt, log, req)
	if err != nil {
		return 0, err
	}

	mp, err := mountBundle(ctx, mounter, snap.PlaceholderPID)
	if err != nil {
		return 0, err
	}
	mountActive := true
	unmount := func() {
		if !mountActive {
			return
		}
		mountActive = false
		// Pass a background context: mp.Unmount has its own internal timeout
		// (nsmount.unmountTimeout) around the ns-bind-mount subprocess.
		if cleanupErr := mp.Unmount(context.Background()); cleanupErr != nil {
			// Deliberately not promoted to retErr. The controller treats any
			// error from Restore as a failed restore and SIGKILLs the placeholder,
			// so surfacing a cleanup failure here would destroy a workload that
			// already restored successfully. Log it and let the pod continue.
			log.Error(cleanupErr, "failed to unmount agent bundle from placeholder namespace")
		}
	}
	defer unmount()

	result, err := execNSRestore(ctx, log, req, snap, mp)
	if err != nil {
		return 0, fmt.Errorf("nsrestore failed: %w", err)
	}
	if err := validateRestoredProcess(snap.TargetRoot, result.RestoredPID, log); err != nil {
		return 0, err
	}

	unmount()
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

func mountBundle(ctx context.Context, mounter Mounter, pid int) (nsmount.MountPoint, error) {
	mp, err := mounter.Mount(ctx, pid)
	if err != nil {
		return nil, fmt.Errorf("mount agent bundle into placeholder: %w", err)
	}
	return mp, nil
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

func inspectRestore(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req RestoreRequest) (*types.RestoreContainerSnapshot, time.Duration, error) {
	if req.CheckpointLocation == "" {
		return nil, 0, fmt.Errorf("checkpoint location is required")
	}

	checkpointPath := req.CheckpointLocation
	baseAbs, err := filepath.Abs(filepath.Dir(checkpointPath))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to resolve checkpoint base path: %w", err)
	}
	checkpointAbs, err := filepath.Abs(checkpointPath)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to resolve checkpoint path: %w", err)
	}
	if checkpointAbs != baseAbs && !strings.HasPrefix(checkpointAbs, baseAbs+string(os.PathSeparator)) {
		return nil, 0, fmt.Errorf("invalid checkpoint id %q", req.CheckpointID)
	}

	m, err := types.ReadManifest(checkpointPath)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read checkpoint manifest: %w", err)
	}

	containerName := req.ContainerName
	if containerName == "" {
		containerName = "main"
	}

	var placeholderPID int
	if req.ContainerID != "" {
		placeholderPID, _, err = rt.ResolveContainer(ctx, req.ContainerID)
	} else {
		placeholderPID, _, err = rt.ResolveContainerByPod(ctx, req.PodName, req.PodNamespace, containerName)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("failed to resolve placeholder container: %w", err)
	}
	log.V(1).Info("Resolved placeholder container", "pid", placeholderPID)

	cgroupRoot, err := snapshotruntime.ResolveCgroupRootFromHostPID(placeholderPID)
	if err != nil {
		log.Error(err, "Failed to resolve placeholder cgroup root; proceeding without explicit cgroup remap")
		cgroupRoot = ""
	}

	cudaDeviceMap := ""
	var gpuDeviceMapDuration time.Duration
	if !m.CUDA.IsEmpty() {
		if len(m.CUDA.SourceGPUUUIDs) == 0 {
			return nil, 0, fmt.Errorf("missing source GPU UUIDs in checkpoint manifest")
		}
		gpuStart := time.Now()
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
			return nil, 0, fmt.Errorf("failed to get target GPU UUIDs: %w", err)
		}
		if len(targetGPUUUIDs) == 0 {
			return nil, 0, fmt.Errorf("missing target GPU UUIDs for %s/%s container %s", req.PodNamespace, req.PodName, containerName)
		}
		cudaDeviceMap, err = cuda.BuildDeviceMap(m.CUDA.SourceGPUUUIDs, targetGPUUUIDs, log)
		gpuDeviceMapDuration = time.Since(gpuStart)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to build CUDA device map: %w", err)
		}
		log.V(1).Info("GPU UUIDs for device map",
			"source_uuids", m.CUDA.SourceGPUUUIDs,
			"target_uuids", targetGPUUUIDs,
			"device_map", cudaDeviceMap,
		)
	}

	return &types.RestoreContainerSnapshot{
		CheckpointPath: checkpointPath,
		PlaceholderPID: placeholderPID,
		TargetRoot:     fmt.Sprintf("%s/%d/root", snapshotruntime.HostProcPath, placeholderPID),
		CgroupRoot:     cgroupRoot,
		CUDADeviceMap:  cudaDeviceMap,
	}, gpuDeviceMapDuration, nil
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
func execNSRestore(ctx context.Context, log logr.Logger, req RestoreRequest, snap *types.RestoreContainerSnapshot, mp nsmount.MountPoint) (*RestoreInNamespaceResult, error) {
	checkpointPath := req.ContainerCheckpointLocation
	if checkpointPath != "" && !filepath.IsAbs(checkpointPath) {
		return nil, fmt.Errorf("container checkpoint location must be absolute: %q", checkpointPath)
	}
	if checkpointPath == "" {
		checkpointPath = snap.CheckpointPath
	}

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
