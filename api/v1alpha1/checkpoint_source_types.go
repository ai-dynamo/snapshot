// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// CheckpointSource publishes what a checkpoint was captured on, so someone
// preparing a restore target can see it without reading the artifact. It is
// projected from the checkpoint manifest and never read back: the restore
// compatibility gates compare the manifest, not this status.
//
// Every field is optional. A fact the agent could not read is absent rather
// than empty, and an artifact captured before a fact was recorded publishes
// without it.
type CheckpointSource struct {
	// Devices records the devices the capture could see, grouped by vendor.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Devices *CheckpointSourceDevices `json:"devices,omitempty"`

	// Node records the machine the checkpoint was captured on.
	// +optional
	Node *CheckpointSourceNode `json:"node,omitempty"`

	// Pod records the container the checkpoint was captured from.
	// +optional
	Pod *CheckpointSourcePod `json:"pod,omitempty"`
}

// CheckpointSourceDevices groups source device facts under one property per
// vendor, so each vendor's payload stays typed and validated and several can be
// present at once without a discriminator. A vendor this CRD has not heard of
// yet is preserved rather than pruned, so a newer agent's facts survive an older
// installed CRD.
type CheckpointSourceDevices struct {
	// Nvidia records the NVIDIA GPUs the capture could see.
	// +optional
	Nvidia *NvidiaCheckpointSource `json:"nvidia,omitempty"`
}

// NvidiaCheckpointSource is what the capture saw of the node's NVIDIA GPUs. The
// field names are the ones the NVIDIA DRA driver publishes on a ResourceSlice,
// so the same GPU reads the same way in both places.
type NvidiaCheckpointSource struct {
	// DriverVersion is the NVIDIA driver the capture ran against.
	// +optional
	DriverVersion string `json:"driverVersion,omitempty"`

	// Instances lists one entry per GPU the captured container could see, so
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

// CheckpointSourceNode is the machine a checkpoint was captured on.
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
}

// CheckpointSourcePod is the container a checkpoint was captured from, and what
// it was given to run in.
type CheckpointSourcePod struct {
	// Image is the container image reference the capture ran.
	// +optional
	Image string `json:"image,omitempty"`

	// ImageDigest identifies which build of the container image the capture
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
