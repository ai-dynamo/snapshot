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
	// Node records the machine the checkpoint was captured on.
	// +optional
	Node *CheckpointSourceNode `json:"node,omitempty"`
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
