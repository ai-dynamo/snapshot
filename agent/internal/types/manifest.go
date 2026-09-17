// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gopkg.in/yaml.v3"

	"github.com/ai-dynamo/snapshot/api/compat"
)

const manifestFilename = "manifest.yaml"

// CuinterposeFormat identifies the host-carrier artifact and matching shim layout.
// Older draft artifacts, including external allocation storage, are not compatible.
const CuinterposeFormat = 2

// CheckpointManifest is saved as manifest.yaml at checkpoint time and loaded at restore.
type CheckpointManifest struct {
	Artifact  ArtifactManifest `yaml:"artifact"`
	CreatedAt time.Time        `yaml:"createdAt"`

	CRIUDump CRIUDumpManifest  `yaml:"criuDump"`
	K8s      SourcePodManifest `yaml:"k8s"`
	Overlay  OverlayManifest   `yaml:"overlay"`
	CUDA     CUDAManifest      `yaml:"cudaRestore,omitempty"`
	Host     HostManifest      `yaml:"host,omitempty"`
	// CUDATools records whether the source ran with Snapshot's CUDA tools
	// (cuda-checkpoint, the cuinterpose shim) delivered into the container.
	CUDATools CUDAToolsManifest `yaml:"cudaTools,omitempty"`
	// Cuinterpose records whether the CUDA interposer shim was in play. It is
	// the restore side's only source of truth: the restore Pod's annotations
	// may be absent or edited, and the state file alone cannot say whether
	// prepare was supposed to have run.
	Cuinterpose CuinterposeManifest `yaml:"cuinterpose,omitempty"`
}

// CuinterposeManifest carries two separate facts about the shim.
type CuinterposeManifest struct {
	// Requested is true when the source Pod opted in. Restore then removes
	// stale shim sockets before CRIU recreates the processes. The shim itself
	// is mounted through the CUDA tools delivery (CUDAToolsManifest).
	Requested bool `yaml:"requested"`
	// Prepared is true when the coordinator's prepare step completed and wrote
	// the state file. Restore then runs the coordinator, and a missing state
	// file is an error rather than a plain restore.
	Prepared bool `yaml:"prepared"`
	Format   int  `yaml:"format,omitempty"`
}

// CUDAToolsManifest is the restore side's source of truth for the tools mount.
type CUDAToolsManifest struct {
	// Delivered is true when the source container mounted the tools volume at
	// podcontract.CUDAToolsMountPath. Restore then bind-mounts the agent's copy
	// at the same path before CRIU runs, because the checkpointed processes
	// have cuda-checkpoint (and possibly the shim) mapped by that path.
	Delivered bool `yaml:"delivered"`
}

// ArtifactManifest pins an on-disk checkpoint to the Kubernetes content object
// and container whose immutable identity determines its path.
type ArtifactManifest struct {
	ContentUID    string `yaml:"contentUID"`
	ContainerName string `yaml:"containerName"`
}

func NewCheckpointManifest(
	contentUID string,
	containerName string,
	criuDump CRIUDumpManifest,
	k8s SourcePodManifest,
	overlay OverlayManifest,
	host HostManifest,
) *CheckpointManifest {
	return &CheckpointManifest{
		Artifact: ArtifactManifest{
			ContentUID:    contentUID,
			ContainerName: containerName,
		},
		CreatedAt: time.Now().UTC(),
		CRIUDump:  criuDump,
		K8s:       k8s,
		Overlay:   overlay,
		Host:      host,
	}
}

// HostManifest records the machine a checkpoint was captured on. A value the
// agent could not read is left out rather than written empty, so it reads as
// unknown instead of as a value that happens to be blank.
type HostManifest struct {
	KernelVersion string `yaml:"kernelVersion,omitempty"`
	CPUArch       string `yaml:"cpuArch,omitempty"`
}

// NewHostManifest takes the architecture from the agent binary rather than
// asking the node: this binary is running on that node, so they agree.
func NewHostManifest(kernelVersion string) HostManifest {
	return HostManifest{
		KernelVersion: kernelVersion,
		CPUArch:       runtime.GOARCH,
	}
}

// CRIUDumpManifest stores the resolved dump-time CRIU mount plan used for restore.
type CRIUDumpManifest struct {
	CRIU     CRIUSettings      `yaml:"criu"`
	ExtMnt   map[string]string `yaml:"extMnt,omitempty"`
	External []string          `yaml:"external,omitempty"`
	SkipMnt  []string          `yaml:"skipMnt,omitempty"`
}

func NewCRIUDumpManifest(criuOpts *criurpc.CriuOpts, settings CRIUSettings) CRIUDumpManifest {
	m := CRIUDumpManifest{CRIU: settings}
	if criuOpts == nil {
		return m
	}

	m.ExtMnt = make(map[string]string, len(criuOpts.ExtMnt))
	for _, mount := range criuOpts.ExtMnt {
		if mount == nil || mount.GetKey() == "" {
			continue
		}
		m.ExtMnt[mount.GetKey()] = mount.GetVal()
	}
	if len(m.ExtMnt) == 0 {
		m.ExtMnt = nil
	}
	m.External = append([]string(nil), criuOpts.External...)
	m.SkipMnt = append([]string(nil), criuOpts.SkipMnt...)
	return m
}

// SourcePodManifest records the source pod identity at checkpoint time.
type SourcePodManifest struct {
	ContainerID  string `yaml:"containerId"`
	PID          int    `yaml:"pid"`
	SourceNode   string `yaml:"sourceNode"`
	PodName      string `yaml:"podName"`
	PodNamespace string `yaml:"podNamespace"`
	PodIP        string `yaml:"podIP,omitempty"`

	// StdioFDs holds readlink targets for FDs 0, 1, 2 (e.g. "pipe:[12345]").
	StdioFDs []string `yaml:"stdioFDs,omitempty"`

	// ImageID is CRI ContainerStatus.image_id, not kubelet's image_ref alias.
	Image       string `yaml:"image,omitempty"`
	ImageID     string `yaml:"imageId,omitempty"`
	CPULimit    string `yaml:"cpuLimit,omitempty"`
	MemoryLimit string `yaml:"memoryLimit,omitempty"`
}

func NewSourcePodManifest(containerID string, pid int, sourceNode, podName, podNamespace, podIP string, stdioFDs []string) SourcePodManifest {
	return SourcePodManifest{
		ContainerID:  containerID,
		PID:          pid,
		SourceNode:   sourceNode,
		PodName:      podName,
		PodNamespace: podNamespace,
		PodIP:        podIP,
		StdioFDs:     append([]string(nil), stdioFDs...),
	}
}

// OverlayManifest holds runtime overlay state captured at checkpoint time.
type OverlayManifest struct {
	Exclusions     OverlaySettings `yaml:"exclusions"`
	UpperDir       string          `yaml:"upperDir,omitempty"`
	ExternalPaths  []string        `yaml:"externalPaths,omitempty"`
	BindMountDests []string        `yaml:"bindMountDests,omitempty"`
}

func NewOverlayManifest(exclusions OverlaySettings, upperDir string, ociSpec *specs.Spec) OverlayManifest {
	meta := OverlayManifest{
		Exclusions: exclusions,
		UpperDir:   upperDir,
	}
	if ociSpec == nil {
		return meta
	}

	if ociSpec.Linux != nil {
		meta.ExternalPaths = make([]string, 0, len(ociSpec.Linux.MaskedPaths)+len(ociSpec.Linux.ReadonlyPaths))
		meta.ExternalPaths = append(meta.ExternalPaths, ociSpec.Linux.MaskedPaths...)
		meta.ExternalPaths = append(meta.ExternalPaths, ociSpec.Linux.ReadonlyPaths...)
	}
	for _, m := range ociSpec.Mounts {
		if m.Type == "bind" {
			meta.BindMountDests = append(meta.BindMountDests, m.Destination)
		}
	}
	return meta
}

// CUDAManifest captures CUDA state from checkpoint time for restore.
type CUDAManifest struct {
	// CustomStorage saves native-owned GPU bytes through PageBroker. Shared
	// cuinterpose allocations still use the creator's host carrier.
	CustomStorage        bool              `yaml:"customStorage,omitempty"`
	PIDs                 []int             `yaml:"pids"`
	SourceGPUUUIDs       []string          `yaml:"sourceGpuUuids"`
	DevicePaths          map[string]string `yaml:"devicePaths,omitempty"`
	NVIDIAVisibleDevices *string           `yaml:"nvidiaVisibleDevices,omitempty"`

	// SourceGPUs describes the same GPUs as SourceGPUUUIDs, in the same order.
	// The UUIDs stay where they are because the device map is built from them
	// and artifacts written before this field exists still restore.
	SourceGPUs          []GPUManifest `yaml:"sourceGpus,omitempty"`
	SourceDriverVersion string        `yaml:"sourceDriverVersion,omitempty"`
}

// GPUManifest is one GPU the checkpointed process could see.
type GPUManifest struct {
	UUID        string `yaml:"uuid"`
	ProductName string `yaml:"productName,omitempty"`
}

func NewCUDAManifest(pids []int, gpus compat.GPUInfo) CUDAManifest {
	m := CUDAManifest{
		PIDs:                append([]int(nil), pids...),
		SourceDriverVersion: gpus.DriverVersion,
	}
	for _, device := range gpus.Devices {
		m.SourceGPUUUIDs = append(m.SourceGPUUUIDs, device.UUID)
		m.SourceGPUs = append(m.SourceGPUs, GPUManifest{
			UUID:        device.UUID,
			ProductName: device.ProductName,
		})
	}
	return m
}

func (m CUDAManifest) IsEmpty() bool {
	return len(m.PIDs) == 0
}

// WriteManifest writes a checkpoint manifest file in the checkpoint directory.
func WriteManifest(checkpointDir string, data *CheckpointManifest) error {
	if data == nil {
		return fmt.Errorf("checkpoint manifest is required")
	}
	if err := validateArtifactManifest(data.Artifact); err != nil {
		return err
	}

	content, err := yaml.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal checkpoint manifest: %w", err)
	}

	manifestPath := filepath.Join(checkpointDir, manifestFilename)
	if err := os.WriteFile(manifestPath, content, 0600); err != nil {
		return fmt.Errorf("failed to write checkpoint manifest: %w", err)
	}

	return nil
}

// ReadManifest reads checkpoint manifest from a checkpoint directory.
func ReadManifest(checkpointDir string) (*CheckpointManifest, error) {
	manifestPath := filepath.Join(checkpointDir, manifestFilename)

	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint manifest: %w", err)
	}

	var data CheckpointManifest
	if err := yaml.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal checkpoint manifest: %w", err)
	}
	if err := validateArtifactManifest(data.Artifact); err != nil {
		return nil, err
	}

	return &data, nil
}

func validateArtifactManifest(artifact ArtifactManifest) error {
	if strings.TrimSpace(artifact.ContentUID) == "" {
		return fmt.Errorf("checkpoint manifest is missing artifact.contentUID")
	}
	if strings.TrimSpace(artifact.ContainerName) == "" {
		return fmt.Errorf("checkpoint manifest is missing artifact.containerName")
	}
	return nil
}

// CompatEnvironment maps the manifest onto what the compatibility gates
// compare. Both gates read it from here, so the two cannot disagree about what
// the checkpoint recorded.
func (m *CheckpointManifest) CompatEnvironment() compat.Environment {
	gpus := m.gpuInfo()
	return compat.Environment{
		KernelVersion:      m.Host.KernelVersion,
		CPUArch:            m.Host.CPUArch,
		Image:              m.K8s.Image,
		ImageID:            m.K8s.ImageID,
		CPULimit:           m.K8s.CPULimit,
		MemoryLimit:        m.K8s.MemoryLimit,
		DriverVersion:      gpus.DriverVersion,
		GPUDevices:         gpus.Devices,
		ExternalizedMounts: m.externalizedMounts(),
	}
}

// WithPodEnvironment records what the captured container ran as. It is the inverse of
// the pod half of CompatEnvironment, and sits next to it so the two field lists cannot
// drift apart.
func (m SourcePodManifest) WithPodEnvironment(env compat.Environment) SourcePodManifest {
	m.Image = env.Image
	m.ImageID = env.ImageID
	m.CPULimit = env.CPULimit
	m.MemoryLimit = env.MemoryLimit
	return m
}

// gpuInfo prefers the described GPUs and falls back to the UUID list, so an
// artifact captured before the models were recorded still reports its GPU count.
func (m *CheckpointManifest) gpuInfo() compat.GPUInfo {
	env := compat.GPUInfo{DriverVersion: m.CUDA.SourceDriverVersion}
	if len(m.CUDA.SourceGPUs) > 0 {
		for _, gpu := range m.CUDA.SourceGPUs {
			env.Devices = append(env.Devices, compat.GPUDevice{
				UUID:        gpu.UUID,
				ProductName: gpu.ProductName,
			})
		}
		return env
	}
	for _, uuid := range m.CUDA.SourceGPUUUIDs {
		env.Devices = append(env.Devices, compat.GPUDevice{UUID: uuid})
	}
	return env
}

// externalizedMounts returns the destinations CRIU externalized at capture, in a
// stable order so a refusal always names them the same way.
func (m *CheckpointManifest) externalizedMounts() []string {
	if len(m.CRIUDump.ExtMnt) == 0 {
		return nil
	}
	destinations := make([]string, 0, len(m.CRIUDump.ExtMnt))
	for destination := range m.CRIUDump.ExtMnt {
		destinations = append(destinations, destination)
	}
	sort.Strings(destinations)
	return destinations
}
