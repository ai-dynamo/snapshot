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

	// nvidiaSMITimeout is what the agent bounds every nsenter nvidia-smi call
	// by. The agent's own context carries no deadline, so a hung one would block
	// the worker for good and cost the node every restore that followed.
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

// DiscoverVisibleGPUs describes the GPUs a container can see, by running
// nvidia-smi inside its mount and PID namespaces. The model and the driver
// version come from the first call and the MIG slices from a second: nothing
// else on the restore path gets to look at the source node's GPUs, so what is
// not read here cannot be compared later.
//
// Every path ends here, and under DRA this is the only path that reports GPUs
// at all, because the kubelet publishes no nvidia.com/gpu devices when the
// NVIDIA DRA driver allocates them instead of the device plugin.
func DiscoverVisibleGPUs(ctx context.Context, hostProcPath string, pid int, timeout time.Duration, log logr.Logger) (compat.GPUInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	output, err := nsenterNvidiaSMI(ctx, hostProcPath, pid,
		"--query-gpu=gpu_uuid,name,driver_version", "--format=csv,noheader",
	)
	if err != nil {
		return compat.GPUInfo{}, fmt.Errorf("nvidia-smi via nsenter (pid %d) failed: %w", pid, err)
	}
	env := parseNvidiaSmiGPUs(string(output))

	// --query-gpu enumerates GPUs, and a MIG slice is not one: a container
	// holding a slice sees the parent card there and nothing of the slice. A
	// failure here costs the slice, not the checkpoint: the device stays
	// recorded as its parent, and an unknown profile admits a restore rather
	// than refusing one.
	listed, err := nsenterNvidiaSMI(ctx, hostProcPath, pid, "-L")
	if err != nil {
		log.V(1).Info("Failed to list MIG devices; recording each GPU as nvidia-smi enumerated it",
			"pid", pid,
			"error", err,
		)
		return env, nil
	}
	return withMIGDevices(env, parseNvidiaSmiMIGDevices(string(listed))), nil
}

func nsenterNvidiaSMI(ctx context.Context, hostProcPath string, pid int, args ...string) ([]byte, error) {
	mountPath := fmt.Sprintf("%s/%d/ns/mnt", strings.TrimRight(hostProcPath, "/"), pid)
	pidPath := fmt.Sprintf("%s/%d/ns/pid", strings.TrimRight(hostProcPath, "/"), pid)
	nsenterArgs := append([]string{
		fmt.Sprintf("--mount=%s", mountPath),
		fmt.Sprintf("--pid=%s", pidPath),
		"--",
		"nvidia-smi",
	}, args...)
	return exec.CommandContext(ctx, "nsenter", nsenterArgs...).Output()
}

// nvidia-smi -L indents each MIG device under its parent GPU as a fixed run of
// whitespace-padded columns. The padding aligns the columns and so varies in
// width, which is why the line is read by column rather than by offset:
//
//	GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-b1c4...)
//	  MIG 3g.40gb     Device  0: (UUID: MIG-7089d0f3-293f-58c9-8f8c-5ea666eedbde)
const (
	migListKind = iota
	migListProfile
	migListDeviceLabel
	migListOrdinal
	migListUUIDLabel
	migListUUID
	migListColumns
)

// migDevice is one MIG slice as nvidia-smi -L lists it under its parent GPU.
type migDevice struct {
	uuid    string
	profile string
}

// parseNvidiaSmiMIGDevices groups the listed MIG slices under the UUID of the
// GPU they were carved from. That nesting is the whole reason the listing is
// worth a second call: it is the only output tying a slice UUID to the parent
// UUID that --query-gpu reports. A parent GPU line opens with its own label
// rather than the MIG one, so a node with MIG disabled yields an empty map
// rather than an error.
func parseNvidiaSmiMIGDevices(output string) map[string][]migDevice {
	byParent := make(map[string][]migDevice)
	var parent string
	for _, line := range strings.Split(output, "\n") {
		columns := strings.Fields(line)
		if len(columns) == 0 {
			continue
		}
		switch columns[migListKind] {
		case "GPU":
			parent = listedUUID(columns)
		case "MIG":
			if parent == "" ||
				len(columns) < migListColumns ||
				columns[migListDeviceLabel] != "Device" ||
				columns[migListUUIDLabel] != "(UUID:" {
				continue
			}
			if _, err := strconv.Atoi(strings.TrimSuffix(columns[migListOrdinal], ":")); err != nil {
				continue
			}
			uuid := columns[migListUUID]
			if !strings.HasSuffix(uuid, ")") {
				continue
			}
			if uuid = strings.TrimSuffix(uuid, ")"); uuid == "" {
				continue
			}
			byParent[parent] = append(byParent[parent], migDevice{uuid: uuid, profile: columns[migListProfile]})
		}
	}
	return byParent
}

// listedUUID reads the "(UUID: <id>)" pair closing a parent GPU line, which sits
// at no fixed column because the product name ahead of it varies in word count.
func listedUUID(columns []string) string {
	for i, column := range columns {
		if column != "(UUID:" || i+1 == len(columns) {
			continue
		}
		if uuid := strings.TrimSuffix(columns[i+1], ")"); uuid != "" {
			return uuid
		}
	}
	return ""
}

// withMIGDevices replaces each parent GPU with the slices carved out of it, so a
// captured slice is recorded under its own UUID and shape rather than under the
// card nvidia-smi enumerated it beneath. The parent's model carries over, that
// being the only name nvidia-smi gives a slice.
func withMIGDevices(env compat.GPUInfo, byParent map[string][]migDevice) compat.GPUInfo {
	if len(byParent) == 0 || len(env.Devices) == 0 {
		return env
	}
	devices := make([]compat.GPUDevice, 0, len(env.Devices))
	for _, device := range env.Devices {
		carved := byParent[device.UUID]
		if len(carved) == 0 {
			devices = append(devices, device)
			continue
		}
		for _, mig := range carved {
			devices = append(devices, compat.GPUDevice{
				UUID:        mig.uuid,
				ProductName: device.ProductName,
				MIGProfile:  mig.profile,
			})
		}
	}
	env.Devices = devices
	return env
}

// parseNvidiaSmiGPUs reads the unquoted CSV nvidia-smi writes. Splitting on
// commas is safe because nvidia-smi documents name and driver_version as
// alphanumeric strings: https://docs.nvidia.com/deploy/nvidia-smi/index.html
// A row it cannot make sense of still contributes its UUID, because the device
// map is built from UUIDs and must not start failing over a model name.
func parseNvidiaSmiGPUs(output string) compat.GPUInfo {
	var env compat.GPUInfo
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, ",", 3)
		uuid := nvidiaSmiValue(fields[0])
		if uuid == "" {
			continue
		}
		device := compat.GPUDevice{UUID: uuid}
		if len(fields) == 3 {
			device.ProductName = nvidiaSmiValue(fields[1])
			if driverVersion := nvidiaSmiValue(fields[2]); driverVersion != "" {
				env.DriverVersion = driverVersion
			}
		}
		env.Devices = append(env.Devices, device)
	}
	return env
}

func nvidiaSmiValue(value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "n/a", "[n/a]", "not supported", "[not supported]":
		return ""
	default:
		return value
	}
}

type visibleGPUDiscovery func(context.Context, string, int, time.Duration, logr.Logger) (compat.GPUInfo, error)

// DiscoverGPUUUIDs resolves GPU UUIDs in the container's runtime ordinal order.
func DiscoverGPUUUIDs(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace, containerName, hostProcPath string, pid int, log logr.Logger) ([]string, error) {
	env, err := DiscoverGPUs(ctx, clientset, podName, podNamespace, containerName, hostProcPath, pid, log)
	if err != nil {
		return nil, err
	}
	return gpuUUIDsOf(env), nil
}

// DiscoverGPUs resolves the same GPUs as DiscoverGPUUUIDs, in the same
// order, described by model and driver version wherever nvidia-smi can be
// reached. Whichever path finds the GPUs, they come out the same shape, so
// what gets recorded does not depend on how this cluster allocates GPUs.
func DiscoverGPUs(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace, containerName, hostProcPath string, pid int, log logr.Logger) (compat.GPUInfo, error) {
	return discoverGPUs(
		ctx,
		clientset,
		podName,
		podNamespace,
		containerName,
		hostProcPath,
		pid,
		nvidiaSMITimeout,
		DiscoverVisibleGPUs,
		log,
	)
}

func discoverGPUs(
	ctx context.Context,
	clientset kubernetes.Interface,
	podName,
	podNamespace,
	containerName,
	hostProcPath string,
	pid int,
	timeout time.Duration,
	discoverVisibleGPUs visibleGPUDiscovery,
	log logr.Logger,
) (compat.GPUInfo, error) {
	gpuUUIDs, hasNVIDIADRAAllocation, err := GetGPUUUIDsViaDRAAPI(ctx, clientset, podName, podNamespace, containerName, log)
	if err != nil {
		if hasNVIDIADRAAllocation {
			return compat.GPUInfo{}, fmt.Errorf("DRA GPU UUID lookup failed: %w", err)
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
			return compat.GPUInfo{}, errors.New(
				"DRA GPU allocation has no resolvable UUIDs",
			)
		}
		visible, err := discoverVisibleGPUs(ctx, hostProcPath, pid, timeout, log)
		if err != nil {
			return compat.GPUInfo{}, fmt.Errorf(
				"discover DRA GPUs in container ordinal order: %w",
				err,
			)
		}
		orderedUUIDs, err := orderDRAUUIDsByRuntime(gpuUUIDs, gpuUUIDsOf(visible))
		if err != nil {
			return compat.GPUInfo{}, err
		}
		log.Info(
			"resolved DRA GPU UUIDs in container ordinal order",
			"uuids", orderedUUIDs,
		)
		return describeGPUs(orderedUUIDs, visible), nil
	}

	gpuUUIDs, err = GetPodGPUUUIDs(ctx, podName, podNamespace, containerName)
	if err != nil {
		return compat.GPUInfo{}, fmt.Errorf("PodResources GPU UUID lookup failed: %w", err)
	}
	if len(gpuUUIDs) > 0 {
		// This path has its GPUs already and needs nvidia-smi only to describe
		// them, so a failure here costs the description, not the checkpoint.
		visible, err := discoverVisibleGPUs(ctx, hostProcPath, pid, timeout, log)
		if err != nil {
			log.V(1).Info("Failed to describe PodResources GPUs; recording their UUIDs alone",
				"pid", pid,
				"error", err,
			)
			return describeGPUs(gpuUUIDs, compat.GPUInfo{}), nil
		}
		return describeGPUs(gpuUUIDs, visible), nil
	}

	log.Info("PodResources API returned no GPU UUIDs, falling back to nvidia-smi", "pid", pid)
	visible, err := discoverVisibleGPUs(ctx, hostProcPath, pid, timeout, log)
	if err != nil {
		return compat.GPUInfo{}, fmt.Errorf("nvidia-smi GPU UUID fallback failed: %w", err)
	}
	log.Info("nvidia-smi fallback discovered GPU UUIDs", "uuids", gpuUUIDsOf(visible))
	return visible, nil
}

// describeGPUs keeps the allocated order and fills each UUID in from what
// nvidia-smi reported about it. A UUID nvidia-smi did not report keeps its
// place undescribed rather than dropping out of the set.
func describeGPUs(uuids []string, visible compat.GPUInfo) compat.GPUInfo {
	described := make(map[string]compat.GPUDevice, len(visible.Devices))
	for _, device := range visible.Devices {
		described[device.UUID] = device
	}
	env := compat.GPUInfo{
		DriverVersion: visible.DriverVersion,
		Devices:       make([]compat.GPUDevice, 0, len(uuids)),
	}
	for _, uuid := range uuids {
		device, ok := described[uuid]
		if !ok {
			device = compat.GPUDevice{UUID: uuid}
		}
		env.Devices = append(env.Devices, device)
	}
	return env
}

func gpuUUIDsOf(env compat.GPUInfo) []string {
	var uuids []string
	for _, device := range env.Devices {
		if device.UUID != "" {
			uuids = append(uuids, device.UUID)
		}
	}
	return uuids
}

// orderDRAUUIDsByRuntime re-sorts allocated UUIDs into the order the container
// sees them. CUDA addresses GPUs by ordinal and a checkpoint records that
// ordering, but the DRA API returns devices in claim order, which need not
// match. A count mismatch is an error rather than a partial ordering.
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
