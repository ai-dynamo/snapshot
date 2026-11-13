// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// CheckpointSource publishes where a snapshot was taken, so someone preparing a
// restore target can see it without reading the snapshot. It is projected from
// the snapshot manifest and never read back: the restore compatibility gates
// compare the manifest, not this status.
//
// Every field is optional. A value the agent could not read is absent rather
// than empty, and a snapshot taken before a value was recorded publishes
// without it.
type CheckpointSource struct {
	// Devices records the devices the checkpoint could see, grouped by vendor.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Devices *CheckpointSourceDevices `json:"devices,omitempty"`

	// Node records the machine the snapshot was taken on.
	// +optional
	Node *CheckpointSourceNode `json:"node,omitempty"`

	// Pod records the container the snapshot was taken from.
	// +optional
	Pod *CheckpointSourcePod `json:"pod,omitempty"`
}

// CheckpointSourceDevices groups source device details under one property per
// vendor, so each vendor's payload stays typed and validated and several can be
// present at once without a discriminator. A vendor this CRD has not heard of
// yet is preserved rather than pruned, so a newer agent's values survive an older
// installed CRD.
type CheckpointSourceDevices struct {
	// Nvidia records the NVIDIA GPUs the checkpoint could see.
	// +optional
	Nvidia *NvidiaCheckpointSource `json:"nvidia,omitempty"`
}

// NvidiaCheckpointSource is what the checkpoint saw of the node's NVIDIA GPUs. The
// field names are the ones the NVIDIA DRA driver publishes on a ResourceSlice,
// so the same GPU reads the same way in both places.
type NvidiaCheckpointSource struct {
	// DriverVersion is the NVIDIA driver the checkpoint ran against.
	// +optional
	DriverVersion string `json:"driverVersion,omitempty"`

	// Instances lists one entry per GPU the checkpointed container could see, so
	// the count is the length of this list.
	// +optional
	Instances []NvidiaCheckpointSourceInstance `json:"instances,omitempty"`
}

// NvidiaCheckpointSourceInstance is one GPU. UUIDs are deliberately left out:
// they identify a physical card rather than describe what a restore target has
// to match.
type NvidiaCheckpointSourceInstance struct {
	// ProductName is the GPU model, as nvidia-smi reports it.
	// +optional
	ProductName string `json:"productName,omitempty"`
}

// CheckpointSourceNode is the machine a snapshot was taken on.
type CheckpointSourceNode struct {
	// Name is the node the source pod ran on.
	// +optional
	Name string `json:"name,omitempty"`

	// Architecture is the node's CPU architecture, as GOARCH spells it.
	// +optional
	Architecture string `json:"architecture,omitempty"`

	// KernelVersion is the node's kernel release.
	// +optional
	KernelVersion string `json:"kernelVersion,omitempty"`

	// CPUFeatures are the instruction-set flags from the node's /proc/cpuinfo
	// that every processor reported, sorted. A restore needs a target that
	// offers all of them.
	// +optional
	CPUFeatures []string `json:"cpuFeatures,omitempty"`
}

// CheckpointSourcePod is the container a snapshot was taken from, and what it
// was given to run in.
type CheckpointSourcePod struct {
	// Image is the container image reference the checkpoint ran.
	// +optional
	Image string `json:"image,omitempty"`

	// ImageDigest identifies which build of the container image the checkpoint
	// ran, which a mutable tag does not.
	// +optional
	ImageDigest string `json:"imageDigest,omitempty"`

	// Memory is the container's memory limit, absent if it had none.
	// +optional
	Memory string `json:"memory,omitempty"`

	// CPU is the container's CPU limit, absent if it had none.
	// +optional
	CPU string `json:"cpu,omitempty"`
}
