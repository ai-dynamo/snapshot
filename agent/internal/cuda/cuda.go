// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cuda provides CUDA checkpoint and restore operations.
package cuda

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// VisibleGPUs is what a container sees of the node's GPUs.
type VisibleGPUs struct {
	// GPUs are the cards in the container's ordinal order, described by
	// model and driver version. A card carved into MIG slices is listed as
	// the card.
	GPUs compat.GPUInfo
	// MIGSlices are the slices listed under those cards. No DiscoverGPUs path
	// can stand in for them: DRA drops a slice's MIG- UUID.
	MIGSlices []string
}

// VisibleGPULister answers what one container sees of the node's GPUs.
type VisibleGPULister func() (VisibleGPUs, error)

// ListVisibleGPUs returns a lister for the container whose host PID is pid. On
// its first call it reads the container's cards from the driver's procfs, or,
// when that cannot answer exactly (MIG), runs nvidia-smi -L inside the
// container's mount and PID namespaces; every later call gets the same answer.
// nvidia-smi spends most of its time initializing NVML, 0.1 to 0.2 s on an idle
// eight-GPU node and seconds while other processes start CUDA, so a capture or
// restore lists once for both its MIG check and GPU discovery.
//
// The model comes from the same source as the UUIDs, and the driver version
// from the kernel module's version file: nothing else on the restore path
// gets to look at the source node's GPUs, so what is not read here cannot be
// compared later. nvidia-smi runs only against the module version it was
// built for, so that file holds the version nvidia-smi would report, and
// every container on the node shares it.
//
// Under DRA this is the only path that reports GPUs at all, because the
// kubelet publishes no nvidia.com/gpu devices when the NVIDIA DRA driver
// allocates them instead of the device plugin.
func ListVisibleGPUs(ctx context.Context, hostProcPath string, pid int) VisibleGPULister {
	return sync.OnceValues(func() (VisibleGPUs, error) {
		if visible, ok := listProcfsGPUs(hostProcPath, pid); ok {
			return visible, nil
		}
		ctx, cancel := context.WithTimeout(ctx, nvidiaSMITimeout)
		defer cancel()

		output, err := nsenterNvidiaSMI(ctx, hostProcPath, pid, "-L")
		if err != nil {
			return VisibleGPUs{}, err
		}
		visible := parseNvidiaSmiList(string(output))
		if len(visible.GPUs.Devices) > 0 {
			visible.GPUs.DriverVersion = readDriverVersion(hostProcPath)
		}
		return visible, nil
	})
}

// listProcfsGPUs reads the cards a container can open from the driver's procfs:
// the cards under its /proc/driver/nvidia/gpus whose /dev/nvidia<minor> it has,
// in PCI bus order, which is the order nvidia-smi -L lists them in. It avoids
// NVML, which brings up every GPU it can open and takes seconds when other
// processes on the node, such as a GMS sidecar, start CUDA at the same time.
// It reports !ok whenever it cannot answer exactly, including when the
// container has MIG capability nodes, because procfs does not list slices.
func listProcfsGPUs(hostProcPath string, pid int) (VisibleGPUs, bool) {
	root := filepath.Join(hostProcPath, strconv.Itoa(pid), "root")
	if caps, err := os.ReadDir(filepath.Join(root, "dev", "nvidia-caps")); err == nil && len(caps) > 0 {
		return VisibleGPUs{}, false
	}
	gpusDir := filepath.Join(root, "proc", "driver", "nvidia", "gpus")
	entries, err := os.ReadDir(gpusDir)
	if err != nil {
		return VisibleGPUs{}, false
	}
	busIDs := make([]string, 0, len(entries))
	devices := map[string]compat.GPUDevice{}
	for _, entry := range entries {
		information, err := os.ReadFile(filepath.Join(gpusDir, entry.Name(), "information"))
		if err != nil {
			return VisibleGPUs{}, false
		}
		fields := parseProcfsGPUInformation(string(information))
		uuid, minor := fields["GPU UUID"], fields["Device Minor"]
		if uuid == "" || minor == "" {
			return VisibleGPUs{}, false
		}
		if _, err := os.Stat(filepath.Join(root, "dev", "nvidia"+minor)); err != nil {
			continue
		}
		busID := strings.ToLower(entry.Name())
		busIDs = append(busIDs, busID)
		devices[busID] = compat.GPUDevice{UUID: uuid, ProductName: fields["Model"]}
	}
	if len(busIDs) == 0 {
		return VisibleGPUs{}, false
	}
	sort.Strings(busIDs)
	var visible VisibleGPUs
	for _, busID := range busIDs {
		visible.GPUs.Devices = append(visible.GPUs.Devices, devices[busID])
	}
	visible.GPUs.DriverVersion = readDriverVersion(hostProcPath)
	return visible, true
}

// parseProcfsGPUInformation reads a card's information file, lines of
// "Key: \t value", such as "GPU UUID: \t GPU-..." and "Device Minor: \t 6".
func parseProcfsGPUInformation(information string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(information, "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fields
}

// readDriverVersion returns the loaded kernel module's version, or nothing
// when the file cannot be read, as when nvidia-smi reports it N/A.
func readDriverVersion(hostProcPath string) string {
	banner, err := os.ReadFile(filepath.Join(hostProcPath, "driver", "nvidia", "version"))
	if err != nil {
		return ""
	}
	return parseDriverVersion(string(banner))
}

// driverVersionPattern reads the open module's banner,
// "NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  615.71.09  Release Build ...",
// and the proprietary one's,
// "NVRM version: NVIDIA UNIX x86_64 Kernel Module  550.54.15  Tue Mar  5 ...".
var driverVersionPattern = regexp.MustCompile(`Kernel Module(?: for \S+)?\s+(\d+(?:\.\d+)+)\s`)

func parseDriverVersion(banner string) string {
	if match := driverVersionPattern.FindStringSubmatch(banner); match != nil {
		return match[1]
	}
	return ""
}

func nsenterNvidiaSMI(ctx context.Context, hostProcPath string, pid int, args ...string) ([]byte, error) {
	mountPath := fmt.Sprintf("%s/%d/ns/mnt", strings.TrimRight(hostProcPath, "/"), pid)
	pidPath := fmt.Sprintf("%s/%d/ns/pid", strings.TrimRight(hostProcPath, "/"), pid)
	cmd := exec.CommandContext(
		ctx,
		"nsenter",
		append([]string{
			fmt.Sprintf("--mount=%s", mountPath),
			fmt.Sprintf("--pid=%s", pidPath),
			"--",
			"nvidia-smi",
		}, args...)...,
	)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi via nsenter (pid %d) failed: %w", pid, err)
	}
	return output, nil
}

// parseNvidiaSmiList reads nvidia-smi -L: a line per card,
// "GPU 0: NVIDIA B200 (UUID: GPU-...)", and nested under it a line per slice,
// "  MIG 1g.10gb Device 0: (UUID: MIG-...)", which becomes "1g.10gb MIG-...".
// A card line without a UUID is not a GPU, because the device map is built
// from UUIDs. A slice line it cannot take apart is still a slice, so it is
// reported as printed rather than dropped.
func parseNvidiaSmiList(output string) VisibleGPUs {
	var visible VisibleGPUs
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		uuid := listedUUID(fields)
		switch fields[0] {
		case "GPU":
			if uuid == "" {
				continue
			}
			device := compat.GPUDevice{UUID: uuid}
			if _, described, ok := strings.Cut(line, ": "); ok {
				name, _, _ := strings.Cut(described, " (UUID:")
				device.ProductName = nvidiaSmiValue(name)
			}
			visible.GPUs.Devices = append(visible.GPUs.Devices, device)
		case "MIG":
			slice := strings.Join(fields, " ")
			if uuid != "" && len(fields) > 1 {
				slice = fields[1] + " " + uuid
			}
			visible.MIGSlices = append(visible.MIGSlices, slice)
		}
	}
	return visible
}

func listedUUID(fields []string) string {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "(UUID:" {
			return strings.TrimSuffix(fields[i+1], ")")
		}
	}
	return ""
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

// DiscoverGPUUUIDs resolves GPU UUIDs in the container's runtime ordinal order.
func DiscoverGPUUUIDs(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace, containerName, hostProcPath string, pid int, log logr.Logger) ([]string, error) {
	env, err := DiscoverGPUs(ctx, clientset, podName, podNamespace, containerName, nil, ListVisibleGPUs(ctx, hostProcPath, pid), log)
	if err != nil {
		return nil, err
	}
	return GPUUUIDs(env), nil
}

// DiscoverGPUs resolves the same GPUs as DiscoverGPUUUIDs, in the same
// order, described by model and driver version wherever nvidia-smi can be
// reached. Whichever path finds the GPUs, they come out the same shape, so
// what gets recorded does not depend on how this cluster allocates GPUs.
// Explicit environment selections are resolved on the host; disabled legacy
// selections inspect only container visibility, which can include CDI devices.
// visible is what the container sees, from ListVisibleGPUs.
func DiscoverGPUs(ctx context.Context, clientset kubernetes.Interface, podName, podNamespace, containerName string, env []string, visible VisibleGPULister, log logr.Logger) (compat.GPUInfo, error) {
	if value := VisibleDevicesValue(env); value != nil && *value != "all" {
		if *value == "" || *value == "none" || *value == "void" {
			// These disable legacy injection, not CDI. Inspect only actual
			// container visibility; never substitute host/allocation UUIDs.
			listed, err := visible()
			return listed.GPUs, err
		}
		return resolveSelectedGPUs(ctx, *value)
	}
	return discoverGPUs(ctx, clientset, podName, podNamespace, containerName, visible, log)
}

func discoverGPUs(
	ctx context.Context,
	clientset kubernetes.Interface,
	podName,
	podNamespace,
	containerName string,
	visible VisibleGPULister,
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
		listed, err := visible()
		if err != nil {
			return compat.GPUInfo{}, fmt.Errorf(
				"discover DRA GPUs in container ordinal order: %w",
				err,
			)
		}
		orderedUUIDs, err := orderDRAUUIDsByRuntime(gpuUUIDs, GPUUUIDs(listed.GPUs))
		if err != nil {
			return compat.GPUInfo{}, err
		}
		log.Info(
			"resolved DRA GPU UUIDs in container ordinal order",
			"uuids", orderedUUIDs,
		)
		return describeGPUs(orderedUUIDs, listed.GPUs), nil
	}

	gpuUUIDs, err = GetPodGPUUUIDs(ctx, podName, podNamespace, containerName)
	if err != nil {
		return compat.GPUInfo{}, fmt.Errorf("PodResources GPU UUID lookup failed: %w", err)
	}
	if len(gpuUUIDs) > 0 {
		// This path has its GPUs already and needs nvidia-smi only to describe
		// them, so a failure here costs the description, not the checkpoint.
		listed, err := visible()
		if err != nil {
			log.V(1).Info("Failed to describe PodResources GPUs; recording their UUIDs alone", "error", err)
			return describeGPUs(gpuUUIDs, compat.GPUInfo{}), nil
		}
		return describeGPUs(gpuUUIDs, listed.GPUs), nil
	}

	log.Info("PodResources API returned no GPU UUIDs, falling back to nvidia-smi")
	listed, err := visible()
	if err != nil {
		return compat.GPUInfo{}, fmt.Errorf("nvidia-smi GPU UUID fallback failed: %w", err)
	}
	log.Info("nvidia-smi fallback discovered GPU UUIDs", "uuids", GPUUUIDs(listed.GPUs))
	return listed.GPUs, nil
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

// GPUUUIDs returns device UUIDs in discovery order.
func GPUUUIDs(env compat.GPUInfo) []string {
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

// FilterProcesses returns the candidate PIDs with a CUDA driver restore thread.
// Driver initialization can create that thread without a cuInterpose core runtime.
// Uses --get-restore-tid (the same technique as the CRIU CUDA plugin) instead of
// --get-state, because --get-state incorrectly matches coordinator processes like
// cuda-checkpoint --launch-job that share a /proc namespace with CUDA processes but
// do not have their own CUDA driver restore thread.
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

// RestoreAndUnlockProcessTree restores and unlocks CUDA state for the given PIDs. It
// runs inside nsrestore and shares its process group so cancellation reaches the CUDA
// helper. helperBinaryPath identifies the binary in that namespace, including through
// an inherited descriptor opened before CRIU restores the workload filesystem.
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
