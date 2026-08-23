// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cuda provides CUDA checkpoint and restore operations.
package cuda

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/kubernetes"
	podresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"

	"github.com/ai-dynamo/snapshot/api/compat"
)

const (
	nvidiaGPUResource  = "nvidia.com/gpu"
	nvidiaGPUDRADriver = "gpu.nvidia.com"

	// HelperBinaryName is the cuda-checkpoint-helper executable name.
	HelperBinaryName = "cuda-checkpoint-helper"
	// DefaultHelperBinaryPath is the agent-side cuda-checkpoint-helper absolute path.
	// In the placeholder namespace pass filepath.Join(bundleDir, HelperBinaryName) instead.
	DefaultHelperBinaryPath = "/usr/local/bin/" + HelperBinaryName

	// nvidiaSMITimeout bounds every nsenter nvidia-smi call. The agent's own
	// context carries no deadline, so a hung one would block the worker for good
	// and cost the node every restore that followed.
	nvidiaSMITimeout = 30 * time.Second
)

var podResourcesSocketPath = "/var/lib/kubelet/pod-resources/kubelet.sock"

var gpuUUIDPattern = regexp.MustCompile(`^GPU-[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

type CheckpointPhaseTimings struct {
	TotalDuration time.Duration
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

// DiscoverVisibleGPUFacts describes the GPUs a container can see, by running
// nvidia-smi inside its mount and PID namespaces. The model and the driver
// version come from the same call as the UUIDs: nothing else on the restore path
// gets to look at the source node's GPUs, so what is not read here cannot be
// compared later.
func DiscoverVisibleGPUFacts(ctx context.Context, hostProcPath string, pid int) (compat.GPUFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, nvidiaSMITimeout)
	defer cancel()

	mountPath := fmt.Sprintf("%s/%d/ns/mnt", strings.TrimRight(hostProcPath, "/"), pid)
	pidPath := fmt.Sprintf("%s/%d/ns/pid", strings.TrimRight(hostProcPath, "/"), pid)
	cmd := exec.CommandContext(
		ctx,
		"nsenter",
		fmt.Sprintf("--mount=%s", mountPath),
		fmt.Sprintf("--pid=%s", pidPath),
		"--",
		"nvidia-smi", "--query-gpu=gpu_uuid,name,driver_version", "--format=csv,noheader",
	)
	output, err := cmd.Output()
	if err != nil {
		return compat.GPUFacts{}, fmt.Errorf("nvidia-smi via nsenter (pid %d) failed: %w", pid, err)
	}
	return parseNvidiaSmiGPUFacts(string(output)), nil
}

// parseNvidiaSmiGPUFacts reads the unquoted CSV nvidia-smi writes. Splitting on
// commas is safe because nvidia-smi documents name and driver_version as
// alphanumeric strings: https://docs.nvidia.com/deploy/nvidia-smi/index.html
// A row it cannot make sense of still contributes its UUID, because the device
// map is built from UUIDs and must not start failing over a model name.
func parseNvidiaSmiGPUFacts(output string) compat.GPUFacts {
	var facts compat.GPUFacts
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ",", 3)
		device := compat.GPUDevice{UUID: strings.TrimSpace(fields[0])}
		if len(fields) == 3 {
			device.ProductName = strings.TrimSpace(fields[1])
			if driverVersion := strings.TrimSpace(fields[2]); driverVersion != "" {
				facts.DriverVersion = driverVersion
			}
		}
		facts.Devices = append(facts.Devices, device)
	}
	return facts
}

type visibleGPUDiscovery func(context.Context, string, int) (compat.GPUFacts, error)

// DiscoverGPUUUIDs resolves GPU UUIDs in the container's runtime ordinal order.
func DiscoverGPUUUIDs(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace, containerName, hostProcPath string, pid int, log logr.Logger) ([]string, error) {
	facts, err := DiscoverGPUFacts(ctx, clientset, podName, podNamespace, containerName, hostProcPath, pid, log)
	if err != nil {
		return nil, err
	}
	return gpuUUIDsOf(facts), nil
}

// DiscoverGPUFacts resolves the same GPUs as DiscoverGPUUUIDs, in the same
// order, described by model and driver version wherever nvidia-smi can be
// reached. Whichever path finds the GPUs, the facts come out the same shape, so
// what gets recorded does not depend on how this cluster allocates GPUs.
func DiscoverGPUFacts(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace, containerName, hostProcPath string, pid int, log logr.Logger) (compat.GPUFacts, error) {
	return discoverGPUFacts(
		ctx,
		clientset,
		podName,
		podNamespace,
		containerName,
		hostProcPath,
		pid,
		DiscoverVisibleGPUFacts,
		log,
	)
}

func discoverGPUFacts(
	ctx context.Context,
	clientset kubernetes.Interface,
	podName,
	podNamespace,
	containerName,
	hostProcPath string,
	pid int,
	discoverVisibleGPUs visibleGPUDiscovery,
	log logr.Logger,
) (compat.GPUFacts, error) {
	gpuUUIDs, hasNVIDIADRAAllocation, err := GetGPUUUIDsViaDRAAPI(ctx, clientset, podName, podNamespace, containerName, log)
	if err != nil {
		if hasNVIDIADRAAllocation {
			return compat.GPUFacts{}, fmt.Errorf("DRA GPU UUID lookup failed: %w", err)
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
			return compat.GPUFacts{}, errors.New(
				"DRA GPU allocation has no resolvable UUIDs",
			)
		}
		visible, err := discoverVisibleGPUs(ctx, hostProcPath, pid)
		if err != nil {
			return compat.GPUFacts{}, fmt.Errorf(
				"discover DRA GPUs in container ordinal order: %w",
				err,
			)
		}
		orderedUUIDs, err := orderDRAUUIDsByRuntime(gpuUUIDs, gpuUUIDsOf(visible))
		if err != nil {
			return compat.GPUFacts{}, err
		}
		log.Info(
			"resolved DRA GPU UUIDs in container ordinal order",
			"uuids", orderedUUIDs,
		)
		return describeGPUs(orderedUUIDs, visible), nil
	}

	gpuUUIDs, err = GetPodGPUUUIDs(ctx, podName, podNamespace, containerName)
	if err != nil {
		return compat.GPUFacts{}, fmt.Errorf("PodResources GPU UUID lookup failed: %w", err)
	}
	if len(gpuUUIDs) > 0 {
		// This path has its GPUs already and needs nvidia-smi only to describe
		// them, so a failure here costs facts, not the checkpoint.
		visible, err := discoverVisibleGPUs(ctx, hostProcPath, pid)
		if err != nil {
			log.V(1).Info("Failed to describe PodResources GPUs; recording their UUIDs alone",
				"pid", pid,
				"error", err,
			)
			return describeGPUs(gpuUUIDs, compat.GPUFacts{}), nil
		}
		return describeGPUs(gpuUUIDs, visible), nil
	}

	log.Info("PodResources API returned no GPU UUIDs, falling back to nvidia-smi", "pid", pid)
	visible, err := discoverVisibleGPUs(ctx, hostProcPath, pid)
	if err != nil {
		return compat.GPUFacts{}, fmt.Errorf("nvidia-smi GPU UUID fallback failed: %w", err)
	}
	log.Info("nvidia-smi fallback discovered GPU UUIDs", "uuids", gpuUUIDsOf(visible))
	return visible, nil
}

// describeGPUs keeps the allocated order and fills each UUID in from what
// nvidia-smi reported about it. A UUID nvidia-smi did not report keeps its
// place undescribed rather than dropping out of the set.
func describeGPUs(uuids []string, visible compat.GPUFacts) compat.GPUFacts {
	described := make(map[string]compat.GPUDevice, len(visible.Devices))
	for _, device := range visible.Devices {
		described[device.UUID] = device
	}
	facts := compat.GPUFacts{
		DriverVersion: visible.DriverVersion,
		Devices:       make([]compat.GPUDevice, 0, len(uuids)),
	}
	for _, uuid := range uuids {
		device, ok := described[uuid]
		if !ok {
			device = compat.GPUDevice{UUID: uuid}
		}
		facts.Devices = append(facts.Devices, device)
	}
	return facts
}

func gpuUUIDsOf(facts compat.GPUFacts) []string {
	var uuids []string
	for _, device := range facts.Devices {
		if device.UUID != "" {
			uuids = append(uuids, device.UUID)
		}
	}
	return uuids
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
func FilterProcesses(ctx context.Context, allPIDs []int, log logr.Logger) []int {
	cudaPIDs := make([]int, 0, len(allPIDs))
	for _, pid := range allPIDs {
		if pid <= 0 {
			continue
		}
		cmd := exec.CommandContext(ctx, DefaultHelperBinaryPath, "--get-restore-tid", "--pid", strconv.Itoa(pid))
		output, err := cmd.CombinedOutput()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.V(1).Info("CUDA restore-tid probe negative", "pid", pid)
			continue
		}
		tid := strings.TrimSpace(string(output))
		log.V(1).Info("CUDA restore-tid probe positive", "pid", pid, "tid", tid)
		cudaPIDs = append(cudaPIDs, pid)
	}
	return cudaPIDs
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

// CheckpointProcessTree locks and checkpoints CUDA state for all given PIDs,
// then persists the launch-job state needed to restore them.
// On failure, the caller is expected to fail the operation and terminate the workload.
func CheckpointProcessTree(ctx context.Context, cudaPIDs []int, jobFile, checkpointDir string, log logr.Logger) (CheckpointPhaseTimings, error) {
	var timings CheckpointPhaseTimings

	start := time.Now()
	for _, pid := range cudaPIDs {
		if err := lockWithJobFile(ctx, pid, jobFile, log); err != nil {
			timings.TotalDuration = time.Since(start)
			return timings, err
		}
	}

	for _, pid := range cudaPIDs {
		if err := checkpointWithJobFile(ctx, pid, jobFile, log); err != nil {
			timings.TotalDuration = time.Since(start)
			return timings, err
		}
	}
	if err := refreshJobFileArtifact(jobFile, checkpointDir); err != nil {
		timings.TotalDuration = time.Since(start)
		return timings, err
	}
	timings.TotalDuration = time.Since(start)

	return timings, nil
}

// RestoreAndUnlockProcessTree restores and unlocks CUDA state for the given PIDs.
// helperBinaryPath must be the absolute path to cuda-checkpoint-helper: DefaultHelperBinaryPath
// on the agent, or filepath.Join(bundleDir, HelperBinaryName) inside the placeholder namespace.
func RestoreAndUnlockProcessTree(ctx context.Context, cudaPIDs []int, deviceMap, helperBinaryPath string, log logr.Logger) (RestorePhaseTimings, error) {
	var timings RestorePhaseTimings

	start := time.Now()
	for _, pid := range cudaPIDs {
		if err := restoreProcess(ctx, pid, deviceMap, helperBinaryPath, log); err != nil {
			timings.TotalDuration = time.Since(start)
			return timings, err
		}
	}

	for _, pid := range cudaPIDs {
		if err := unlock(ctx, pid, helperBinaryPath, log); err != nil {
			timings.TotalDuration = time.Since(start)
			state, stateErr := getState(ctx, pid, helperBinaryPath)
			if stateErr == nil && state == "running" {
				log.Info("cuda-checkpoint-helper unlock returned error but process is already running", "pid", pid)
				continue
			}
			return timings, err
		}
	}
	timings.TotalDuration = time.Since(start)

	return timings, nil
}
