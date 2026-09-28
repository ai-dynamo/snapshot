// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// CheckpointStorageBinding identifies the configured store owning a content's
// publications. It is not a transfer-engine choice or a credential reference.
type CheckpointStorageBinding struct {
	// StoreID is the versioned canonical storage identity shared with PageBroker.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^store-v1-[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=73
	StoreID string `json:"storeID"`
}

// CheckpointStorageStatus holds confirmed publications. The store ID comes from
// immutable spec.storage; handles are meaningful only in that bound store.
type CheckpointStorageStatus struct {
	// Artifacts contains one descriptor per confirmed container. It can be
	// partial during capture/recovery; Ready still requires all captures to finish.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=containerName
	Artifacts []PublishedContainerArtifact `json:"artifacts"`
}

// PublishedContainerArtifact is the persisted part of PageBroker's
// PublishedArtifact, associated with a captured container. Readers pass the
// handle and version back unchanged; they must not construct filesystem paths.
type PublishedContainerArtifact struct {
	// ContainerName selects the captured container within the content.
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

	// ArtifactFormatVersion selects the reader together with the store backend.
	// Unknown nonempty versions remain recordable; readers report unsupported ones.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`\S`
	ArtifactFormatVersion string `json:"artifactFormatVersion"`
}
