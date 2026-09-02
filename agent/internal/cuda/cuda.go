// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cuda provides CUDA checkpoint and restore operations.
package cuda

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	podresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

const (
	nvidiaGPUResource  = "nvidia.com/gpu"
	nvidiaGPUDRADriver = "gpu.nvidia.com"

	// HelperBinaryName is the cuda-checkpoint-helper executable name.
	HelperBinaryName = "cuda-checkpoint-helper"
	// DefaultHelperBinaryPath is the agent-side cuda-checkpoint-helper absolute path.
	// In the placeholder namespace pass filepath.Join(bundleDir, HelperBinaryName) instead.
	DefaultHelperBinaryPath = "/usr/local/bin/" + HelperBinaryName
)

var podResourcesSocketPath = "/var/lib/kubelet/pod-resources/kubelet.sock"

var gpuUUIDPattern = regexp.MustCompile(`^GPU-[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

// Each Snapshot agent process deliberately executes one CUDA operation at a time.
// Hold the slot across each process-tree sequence so checkpoints/restores from
// different workloads cannot interleave lock, transfer, and unlock requests.
var cudaOperationSlot = make(chan struct{}, 1)

func acquireCUDAOperation(ctx context.Context) error {
	select {
	case cudaOperationSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for process-local CUDA operation slot: %w", context.Cause(ctx))
	}
}

func acquireCUDAOperationLogged(ctx context.Context, log logr.Logger) error {
	started := time.Now()
	err := acquireCUDAOperation(ctx)
	log.Info("CUDA operation slot wait finished", "wait_duration", time.Since(started), "acquired", err == nil)
	return err
}

func releaseCUDAOperation() { <-cudaOperationSlot }

type CheckpointPhaseTimings struct {
	TotalDuration time.Duration
}

type checkpointOperationError struct {
	err                error
	targetMayBeMutated bool
}

func (e *checkpointOperationError) Error() string { return e.err.Error() }
func (e *checkpointOperationError) Unwrap() error { return e.err }

// FailedBeforeTargetMutation is true only when the helper could not be
// contacted before any CUDA lock succeeded. Unknown RPC outcomes and CUDA
// failures remain conservatively classified as possibly mutating the target.
func FailedBeforeTargetMutation(err error) bool {
	var operationErr *checkpointOperationError
	return errors.As(err, &operationErr) && !operationErr.targetMayBeMutated
}

// LockAndCheckpointProcessTreeValidated locks and checkpoints the identities
// captured before any destructive CUDA operation and revalidates each identity
// immediately before every driver call.
func LockAndCheckpointProcessTreeValidated(
	ctx context.Context,
	processes []snapshotruntime.ProcessDetails,
	jobFile,
	storageMode,
	checkpointDir string,
	gpuUUIDs []string,
	transferSettings types.CUDATransferSettings,
	log logr.Logger,
) (CheckpointPhaseTimings, error) {
	if err := validateCUDAOperationBudget(ctx, actionCheckpoint, len(processes)); err != nil {
		return CheckpointPhaseTimings{}, &checkpointOperationError{
			err:                err,
			targetMayBeMutated: false,
		}
	}
	if err := acquireCUDAOperationLogged(ctx, log); err != nil {
		return CheckpointPhaseTimings{}, &checkpointOperationError{
			err:                err,
			targetMayBeMutated: false,
		}
	}
	defer releaseCUDAOperation()
	pids, targetIDs, identities, err := validatedProcessIdentities(processes)
	if err != nil {
		return CheckpointPhaseTimings{}, &checkpointOperationError{
			err:                err,
			targetMayBeMutated: false,
		}
	}
	return lockAndCheckpointProcessTree(
		ctx,
		pids,
		targetIDs,
		jobFile,
		storageMode,
		checkpointDir,
		gpuUUIDs,
		transferSettings,
		identityValidatingRunner{
			runner:     commandHelperActionRunner{},
			procRoot:   snapshotruntime.HostProcPath,
			identities: identities,
		},
		log,
	)
}

func validatedProcessIdentities(processes []snapshotruntime.ProcessDetails) ([]int, []int, map[int]snapshotruntime.ProcessDetails, error) {
	pids := make([]int, 0, len(processes))
	targetIDs := make([]int, 0, len(processes))
	identities := make(map[int]snapshotruntime.ProcessDetails, len(processes))
	seenTargetIDs := make(map[int]struct{}, len(processes))
	for _, process := range processes {
		if process.OutermostPID <= 0 || process.InnermostPID <= 0 || process.StartTimeTicks == 0 || process.Cgroup == "" {
			return nil, nil, nil, fmt.Errorf("invalid host process identity")
		}
		if _, exists := identities[process.OutermostPID]; exists {
			return nil, nil, nil, fmt.Errorf("duplicate host PID %d", process.OutermostPID)
		}
		if _, exists := seenTargetIDs[process.InnermostPID]; exists {
			return nil, nil, nil, fmt.Errorf("duplicate CUDA target namespace PID %d", process.InnermostPID)
		}
		pids = append(pids, process.OutermostPID)
		targetIDs = append(targetIDs, process.InnermostPID)
		identities[process.OutermostPID] = process
		seenTargetIDs[process.InnermostPID] = struct{}{}
	}
	return pids, targetIDs, identities, nil
}

type RestorePhaseTimings struct {
	TotalDuration time.Duration
}

// GetPodGPUUUIDs resolves GPU UUIDs for a pod/container from kubelet
// PodResources (nvidia.com/gpu entries in GetDevices()).
func GetPodGPUUUIDs(ctx context.Context, podName, podNamespace, containerName string) ([]string, error) {
	if podName == "" || podNamespace == "" {
		return nil, nil
	}

	conn, err := grpc.NewClient(
		"unix://"+podResourcesSocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := podresourcesv1.NewPodResourcesListerClient(conn)
	resp, err := client.List(ctx, &podresourcesv1.ListPodResourcesRequest{})
	if err != nil {
		return nil, err
	}

	var uuids []string
	for _, pod := range resp.GetPodResources() {
		if pod.GetName() != podName || pod.GetNamespace() != podNamespace {
			continue
		}
		for _, container := range pod.GetContainers() {
			if containerName != "" && container.GetName() != containerName {
				continue
			}
			for _, device := range container.GetDevices() {
				if device.GetResourceName() == nvidiaGPUResource {
					uuids = append(uuids, device.GetDeviceIds()...)
				}
			}

		}
	}

	return uuids, nil
}

// GetGPUUUIDsViaNvidiaSmi discovers GPU UUIDs by running nvidia-smi inside the
// container's mount and PID namespaces. This is the fallback path when the kubelet
// PodResources API does not report GPU devices (e.g. when GPUs are allocated
// via DRA instead of the NVIDIA device plugin).
func GetGPUUUIDsViaNvidiaSmi(ctx context.Context, hostProcPath string, pid int) ([]string, error) {
	mountPath := fmt.Sprintf("%s/%d/ns/mnt", strings.TrimRight(hostProcPath, "/"), pid)
	pidPath := fmt.Sprintf("%s/%d/ns/pid", strings.TrimRight(hostProcPath, "/"), pid)
	cmd := exec.CommandContext(
		ctx,
		"nsenter",
		fmt.Sprintf("--mount=%s", mountPath),
		fmt.Sprintf("--pid=%s", pidPath),
		"--",
		"nvidia-smi", "--query-gpu=gpu_uuid", "--format=csv,noheader",
	)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi via nsenter (pid %d) failed: %w", pid, err)
	}
	var uuids []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			uuids = append(uuids, line)
		}
	}
	return uuids, nil
}

type visibleGPUDiscovery func(context.Context, string, int) ([]string, error)

// DiscoverGPUUUIDs resolves GPU UUIDs in the container's runtime ordinal order.
func DiscoverGPUUUIDs(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace, containerName, hostProcPath string, pid int, log logr.Logger) ([]string, error) {
	return discoverGPUUUIDs(
		ctx,
		clientset,
		podName,
		podNamespace,
		containerName,
		hostProcPath,
		pid,
		GetGPUUUIDsViaNvidiaSmi,
		log,
	)
}

func discoverGPUUUIDs(
	ctx context.Context,
	clientset kubernetes.Interface,
	podName,
	podNamespace,
	containerName,
	hostProcPath string,
	pid int,
	discoverVisibleGPUs visibleGPUDiscovery,
	log logr.Logger,
) ([]string, error) {
	gpuUUIDs, hasNVIDIADRAAllocation, err := GetGPUUUIDsViaDRAAPI(ctx, clientset, podName, podNamespace, containerName, log)
	if err != nil {
		if hasNVIDIADRAAllocation {
			return nil, fmt.Errorf("DRA GPU UUID lookup failed: %w", err)
		}
		log.Error(
			err,
			"DRA API GPU UUID lookup failed, trying other discovery paths",
			"pod", podNamespace+"/"+podName,
		)
		gpuUUIDs = nil
	}

	if hasNVIDIADRAAllocation {
		if len(gpuUUIDs) == 0 {
			return nil, errors.New(
				"DRA GPU allocation has no resolvable UUIDs",
			)
		}
		visibleGPUUUIDs, err := discoverVisibleGPUs(ctx, hostProcPath, pid)
		if err != nil {
			return nil, fmt.Errorf(
				"discover DRA GPUs in container ordinal order: %w",
				err,
			)
		}
		orderedUUIDs, err := orderDRAUUIDsByRuntime(gpuUUIDs, visibleGPUUUIDs)
		if err != nil {
			return nil, err
		}
		log.Info(
			"resolved DRA GPU UUIDs in container ordinal order",
			"uuids", orderedUUIDs,
		)
		return orderedUUIDs, nil
	}

	gpuUUIDs, err = GetPodGPUUUIDs(ctx, podName, podNamespace, containerName)
	if err != nil {
		return nil, fmt.Errorf("PodResources GPU UUID lookup failed: %w", err)
	}
	if len(gpuUUIDs) > 0 {
		return gpuUUIDs, nil
	}

	log.Info("PodResources API returned no GPU UUIDs, falling back to nvidia-smi", "pid", pid)
	gpuUUIDs, err = discoverVisibleGPUs(ctx, hostProcPath, pid)
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi GPU UUID fallback failed: %w", err)
	}
	log.Info("nvidia-smi fallback discovered GPU UUIDs", "uuids", gpuUUIDs)
	return gpuUUIDs, nil
}

func orderDRAUUIDsByRuntime(allocatedUUIDs, visibleUUIDs []string) ([]string, error) {
	if len(allocatedUUIDs) != len(visibleUUIDs) {
		return nil, fmt.Errorf(
			"DRA allocation and container-visible GPU count differ: allocated=%d visible=%d",
			len(allocatedUUIDs),
			len(visibleUUIDs),
		)
	}

	allocated := make(map[string]struct{}, len(allocatedUUIDs))
	for _, uuid := range allocatedUUIDs {
		if !gpuUUIDPattern.MatchString(uuid) {
			return nil, fmt.Errorf("DRA allocation contains invalid GPU UUID %q", uuid)
		}
		if _, duplicate := allocated[uuid]; duplicate {
			return nil, fmt.Errorf("DRA allocation contains duplicate GPU UUID %q", uuid)
		}
		allocated[uuid] = struct{}{}
	}

	seen := make(map[string]struct{}, len(visibleUUIDs))
	for _, uuid := range visibleUUIDs {
		if !gpuUUIDPattern.MatchString(uuid) {
			return nil, fmt.Errorf("container reports invalid GPU UUID %q", uuid)
		}
		if _, duplicate := seen[uuid]; duplicate {
			return nil, fmt.Errorf("container reports duplicate GPU UUID %q", uuid)
		}
		if _, ok := allocated[uuid]; !ok {
			return nil, fmt.Errorf(
				"container-visible GPU %q is not in the DRA allocation",
				uuid,
			)
		}
		seen[uuid] = struct{}{}
	}

	return append([]string(nil), visibleUUIDs...), nil
}

// FilterProcesses returns the subset of candidate PIDs that hold actual CUDA contexts.
// Uses --get-restore-tid (the same technique as the CRIU CUDA plugin) instead of
// --get-state, because --get-state incorrectly matches coordinator processes like
// cuda-checkpoint --launch-job that share a /proc namespace with CUDA processes but
// don't hold CUDA contexts themselves.
func parseRestoreTIDProbeOutput(output []byte) (bool, error) {
	value := strings.TrimSpace(string(output))
	if value == "none" {
		return false, nil
	}
	tid, err := strconv.Atoi(value)
	if err != nil || tid <= 0 {
		return false, fmt.Errorf("invalid CUDA restore-tid probe response %q", value)
	}
	return true, nil
}

func FilterProcesses(ctx context.Context, allPIDs []int, log logr.Logger) ([]int, error) {
	if err := acquireCUDAOperationLogged(ctx, log); err != nil {
		return nil, fmt.Errorf("acquire process-local CUDA operation slot for restore-tid discovery: %w", err)
	}
	defer releaseCUDAOperation()
	cudaPIDs := make([]int, 0, len(allPIDs))
	for _, pid := range allPIDs {
		if pid <= 0 {
			continue
		}
		cmd := exec.CommandContext(ctx, DefaultHelperBinaryPath, "--get-restore-tid", "--pid", strconv.Itoa(pid))
		output, err := cmd.CombinedOutput()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("discover CUDA process identity: %w", context.Cause(ctx))
			}
			return nil, fmt.Errorf("CUDA restore-tid probe failed for PID %d: %w (output: %s)", pid, err, strings.TrimSpace(string(output)))
		}
		ownsCUDAState, err := parseRestoreTIDProbeOutput(output)
		if err != nil {
			return nil, fmt.Errorf("CUDA restore-tid probe failed for PID %d: %w", pid, err)
		}
		if !ownsCUDAState {
			log.V(1).Info("CUDA restore-tid probe negative", "pid", pid)
			continue
		}
		log.V(1).Info("CUDA restore-tid probe positive", "pid", pid, "tid", strings.TrimSpace(string(output)))
		cudaPIDs = append(cudaPIDs, pid)
	}
	return cudaPIDs, nil
}

// BuildDeviceMap creates a cuda-checkpoint-helper --device-map value from source and target GPU UUID lists.
// When a source UUID exists in the target set, it maps to itself (identity mapping) to avoid
// unnecessary cross-GPU restore on same-node restores where kubelet returns GPUs in different order.
// Remaining unmatched source UUIDs are paired with remaining unmatched target UUIDs positionally.
// If all mappings are identity mappings, it returns an empty string so same-GPU restores use the
// default CUDA restore path instead of forcing the GPU migration path.
func BuildDeviceMap(sourceUUIDs, targetUUIDs []string, log logr.Logger) (string, error) {
	if len(sourceUUIDs) != len(targetUUIDs) {
		return "", fmt.Errorf("GPU count mismatch: source has %d, target has %d", len(sourceUUIDs), len(targetUUIDs))
	}
	if len(sourceUUIDs) == 0 {
		return "", fmt.Errorf("GPU UUID list is empty")
	}
	log.V(1).Info("BuildDeviceMap inputs", "source_uuids", sourceUUIDs, "target_uuids", targetUUIDs)

	targetSet := make(map[string]bool, len(targetUUIDs))
	for _, t := range targetUUIDs {
		targetSet[t] = true
	}

	// First pass: identity-map any source UUID that exists in the target set
	mapping := make(map[string]string, len(sourceUUIDs))
	usedTargets := make(map[string]bool, len(targetUUIDs))
	for _, src := range sourceUUIDs {
		if targetSet[src] {
			mapping[src] = src
			usedTargets[src] = true
		}
	}

	// Second pass: pair remaining source UUIDs with remaining target UUIDs positionally
	var remainingTargets []string
	for _, t := range targetUUIDs {
		if !usedTargets[t] {
			remainingTargets = append(remainingTargets, t)
		}
	}
	idx := 0
	for _, src := range sourceUUIDs {
		if _, ok := mapping[src]; !ok {
			mapping[src] = remainingTargets[idx]
			idx++
		}
	}

	allIdentity := true
	for _, src := range sourceUUIDs {
		if mapping[src] != src {
			allIdentity = false
			break
		}
	}
	if allIdentity {
		return "", nil
	}

	pairs := make([]string, len(sourceUUIDs))
	for i, src := range sourceUUIDs {
		pairs[i] = src + "=" + mapping[src]
	}
	return strings.Join(pairs, ","), nil
}

func validateTransferSettings(transferSettings types.CUDATransferSettings) (types.CUDATransferSettings, error) {
	transferSettings = transferSettings.WithDefaults()
	if err := transferSettings.Validate(); err != nil {
		return types.CUDATransferSettings{}, fmt.Errorf("invalid CUDA transfer settings: %w", err)
	}
	return transferSettings, nil
}

func lockAndCheckpointProcessTree(
	ctx context.Context,
	cudaPIDs []int,
	targetIDs []int,
	jobFile,
	storageMode,
	checkpointDir string,
	gpuUUIDs []string,
	transferSettings types.CUDATransferSettings,
	runner helperActionRunner,
	log logr.Logger,
) (CheckpointPhaseTimings, error) {
	// Once any target has been locked, an error is intentionally not recovered
	// in this package. The caller must treat the process tree as unsafe and
	// terminate it after revalidating every process identity. Unlocking only a
	// subset could resume a workload whose CUDA targets no longer agree.
	var timings CheckpointPhaseTimings
	var err error
	transferSettings, err = validateTransferSettings(transferSettings)
	if err != nil {
		return timings, err
	}
	if storageMode == types.CUDAStorageModePOSIX && len(targetIDs) != len(cudaPIDs) {
		return timings, fmt.Errorf("CUDA target identity count %d does not match PID count %d", len(targetIDs), len(cudaPIDs))
	}

	start := time.Now()
	locked := 0
	for _, pid := range cudaPIDs {
		if err := runner.run(ctx, helperAction{PID: pid, Action: actionLock, StorageMode: storageMode, JobFile: jobFile, Transfer: transferSettings}, log); err != nil {
			timings.TotalDuration = time.Since(start)
			return timings, &checkpointOperationError{
				err: err,
				targetMayBeMutated: locked > 0 ||
					(!errors.Is(err, errDaemonUnavailable) &&
						!errors.Is(err, errCheckpointLockNotAcquired) &&
						!errors.Is(err, errProcessIdentityChangedBeforeCUDA)),
			}
		}
		locked++
	}

	for index, pid := range cudaPIDs {
		processDir := ""
		var selectedDevices []string
		if storageMode == types.CUDAStorageModePOSIX {
			processDir = customStorageProcessDir(checkpointDir, targetIDs[index])
			selectedDevices = gpuUUIDs
		}
		if err := runner.run(ctx, helperAction{PID: pid, Action: actionCheckpoint, StorageMode: storageMode, StorageDir: processDir, JobFile: jobFile, GPUUUIDs: selectedDevices, Transfer: transferSettings}, log); err != nil {
			timings.TotalDuration = time.Since(start)
			return timings, &checkpointOperationError{err: err, targetMayBeMutated: true}
		}
	}
	if err := refreshJobFileArtifact(jobFile, checkpointDir); err != nil {
		timings.TotalDuration = time.Since(start)
		return timings, &checkpointOperationError{err: err, targetMayBeMutated: true}
	}
	timings.TotalDuration = time.Since(start)

	return timings, nil
}

func customStorageProcessDir(checkpointDir string, namespacePID int) string {
	return filepath.Join(checkpointDir, "cuda-custom-storage", fmt.Sprintf("process-nspid-%d", namespacePID))
}

// RestoreAndUnlockProcessTreeValidated restores and unlocks identities resolved
// after CRIU and revalidates them immediately before every CUDA driver call.
func RestoreAndUnlockProcessTreeValidated(
	ctx context.Context,
	processes []snapshotruntime.ProcessDetails,
	deviceMap,
	storageMode,
	checkpointDir,
	jobFile string,
	targetGPUUUIDs []string,
	transferSettings types.CUDATransferSettings,
	log logr.Logger,
) (RestorePhaseTimings, error) {
	if err := validateCUDAOperationBudget(ctx, actionRestore, len(processes)); err != nil {
		return RestorePhaseTimings{}, err
	}
	if err := acquireCUDAOperationLogged(ctx, log); err != nil {
		return RestorePhaseTimings{}, err
	}
	defer releaseCUDAOperation()
	pids, targetIDs, identities, err := validatedProcessIdentities(processes)
	if err != nil {
		return RestorePhaseTimings{}, err
	}
	return restoreAndUnlockProcessTree(
		ctx,
		pids,
		targetIDs,
		deviceMap,
		storageMode,
		checkpointDir,
		jobFile,
		targetGPUUUIDs,
		transferSettings,
		identityValidatingRunner{
			runner:     commandHelperActionRunner{},
			procRoot:   snapshotruntime.HostProcPath,
			identities: identities,
		},
		log,
	)
}

func restoreAndUnlockProcessTree(
	ctx context.Context,
	cudaPIDs []int,
	targetIDs []int,
	deviceMap,
	storageMode,
	checkpointDir,
	jobFile string,
	targetGPUUUIDs []string,
	transferSettings types.CUDATransferSettings,
	runner helperActionRunner,
	log logr.Logger,
) (RestorePhaseTimings, error) {
	var timings RestorePhaseTimings
	var err error
	transferSettings, err = validateTransferSettings(transferSettings)
	if err != nil {
		return timings, err
	}
	if storageMode == types.CUDAStorageModePOSIX && len(targetIDs) != len(cudaPIDs) {
		return timings, fmt.Errorf("CUDA target identity count %d does not match PID count %d", len(targetIDs), len(cudaPIDs))
	}

	start := time.Now()
	for index, pid := range cudaPIDs {
		processDir := ""
		var selectedDevices []string
		requestJobFile := jobFile
		if jobFile != "" {
			requestJobFile, err = HostJobFilePath(pid)
			if err != nil {
				timings.TotalDuration = time.Since(start)
				return timings, err
			}
		}
		if storageMode == types.CUDAStorageModePOSIX {
			processDir = customStorageProcessDir(checkpointDir, targetIDs[index])
			selectedDevices = targetGPUUUIDs
		}
		if err := runner.run(ctx, helperAction{PID: pid, Action: actionRestore, DeviceMap: deviceMap, StorageMode: storageMode, StorageDir: processDir, JobFile: requestJobFile, GPUUUIDs: selectedDevices, Transfer: transferSettings}, log); err != nil {
			timings.TotalDuration = time.Since(start)
			return timings, err
		}
	}

	for _, pid := range cudaPIDs {
		requestJobFile := jobFile
		if jobFile != "" {
			requestJobFile, err = HostJobFilePath(pid)
			if err != nil {
				timings.TotalDuration = time.Since(start)
				return timings, err
			}
		}
		if err := runner.run(ctx, helperAction{PID: pid, Action: actionUnlock, StorageMode: storageMode, JobFile: requestJobFile, Transfer: transferSettings}, log); err != nil {
			timings.TotalDuration = time.Since(start)
			return timings, err
		}
	}
	timings.TotalDuration = time.Since(start)

	return timings, nil
}
