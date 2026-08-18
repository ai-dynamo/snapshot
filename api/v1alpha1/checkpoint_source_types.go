// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// CheckpointSourceType names the vendor whose source facts a CheckpointSource carries.
type CheckpointSourceType string

// CheckpointSourceTypeNvidia selects the NVIDIA source facts.
const CheckpointSourceTypeNvidia CheckpointSourceType = "Nvidia"

// CheckpointSource records source workload facts needed to configure a restore.
// Type names the vendor whose facts are recorded, and the matching vendor field
// carries them, so a vendor can record its own fact set without disturbing another's.
// +kubebuilder:validation:XValidation:rule="(self.type == 'Nvidia') == has(self.nvidia)",message="source payload must match type"
type CheckpointSource struct {
	// Type names the vendor whose source facts are recorded.
	// +optional
	// +kubebuilder:default=Nvidia
	// +kubebuilder:validation:Enum=Nvidia
	Type CheckpointSourceType `json:"type,omitempty"`

	// Nvidia records the NVIDIA source facts.
	// +optional
	Nvidia *NvidiaCheckpointSource `json:"nvidia,omitempty"`
}

// NvidiaCheckpointSource records the NVIDIA source facts needed to configure a restore.
type NvidiaCheckpointSource struct {
	// Hardware records source GPU resources.
	Hardware *CheckpointSourceHardware `json:"hardware,omitempty"`

	// Node is the source node name.
	Node string `json:"node,omitempty"`

	// DeclaredVolumes lists source workload volume declarations required for restore.
	DeclaredVolumes []CheckpointSourceDeclaredVolume `json:"declaredVolumes,omitempty"`

	// DeclaredVolumeCount is the number of declared volumes required for restore.
	// +kubebuilder:validation:Minimum=0
	DeclaredVolumeCount *int32 `json:"declaredVolumeCount,omitempty"`
}

// CheckpointSourceHardware records source GPU resources.
type CheckpointSourceHardware struct {
	// GPUCount is the number of source GPUs.
	// +kubebuilder:validation:Minimum=0
	GPUCount *int32 `json:"gpuCount,omitempty"`

	// GPUs identifies the source GPUs.
	GPUs []CheckpointSourceGPU `json:"gpus,omitempty"`
}

// CheckpointSourceGPU identifies a source GPU.
type CheckpointSourceGPU struct {
	// UUID is the source GPU UUID.
	UUID string `json:"uuid,omitempty"`
}

// CheckpointSourceDeclaredVolume identifies a source workload volume required for restore.
type CheckpointSourceDeclaredVolume struct {
	// Path is where the volume was exposed in the source container.
	Path string `json:"path" yaml:"path"`

	// Volume is the name from the source pod's volume declarations.
	Volume string `json:"volume" yaml:"volume"`

	// VolumeSource identifies the original source as kind[/identifier].
	VolumeSource string `json:"volumeSource" yaml:"volumeSource"`
}
