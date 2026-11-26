// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// CheckpointStorageBinding identifies the configured store owning a content's
// publications. It is not a transfer-engine choice or a credential reference.
// +kubebuilder:validation:XValidation:rule="!has(self.snapshotStoreName) || has(self.reclaimPolicy)",message="reclaimPolicy is required when snapshotStoreName is set"
type CheckpointStorageBinding struct {
	// StoreID is the versioned canonical storage identity shared with PageBroker.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^store-v1-[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=73
	StoreID string `json:"storeID"`

	// SnapshotStoreName is the SnapshotStore the content was bound to.
	// StoreID says where the data is; the store says how to reach it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +optional
	SnapshotStoreName string `json:"snapshotStoreName,omitempty"`

	// ReclaimPolicy is copied from the store when the content is bound. It does
	// not change afterwards.
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
}

// CheckpointStorageStatus holds confirmed publications. The store ID comes from
// immutable spec.storage; handles are meaningful only in that bound store.
type CheckpointStorageStatus struct {
	// Artifacts contains one descriptor per confirmed container. It can be
	// partial during a checkpoint or recovery; Ready still requires every container to finish.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=containerName
	Artifacts []PublishedContainerArtifact `json:"artifacts"`
}

// PublishedContainerArtifact is the persisted part of PageBroker's
// PublishedArtifact, associated with a checkpointed container. Readers pass the
// handle back unchanged; they must not construct filesystem paths.
type PublishedContainerArtifact struct {
	// ContainerName selects the checkpointed container within the content.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ContainerName string `json:"containerName"`

	// ArtifactHandle is an opaque locator within the bound store.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:Pattern=`\S`
	ArtifactHandle string `json:"artifactHandle"`
}
