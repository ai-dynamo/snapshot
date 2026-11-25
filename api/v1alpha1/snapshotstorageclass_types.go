// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReclaimPolicy says what happens to a snapshot's data when its
// PodSnapshotContent is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type ReclaimPolicy string

const (
	// ReclaimPolicyDelete removes the snapshot's data with its content.
	ReclaimPolicyDelete ReclaimPolicy = "Delete"
	// ReclaimPolicyRetain keeps the snapshot's data after its content is deleted.
	ReclaimPolicyRetain ReclaimPolicy = "Retain"
)

// SnapshotStorageClassSpec describes one snapshot store. Exactly one store
// block is set, and the block that is set names the kind of store.
// +kubebuilder:validation:XValidation:rule="has(self.pvc) != has(self.s3)",message="exactly one of pvc or s3 must be set"
// +kubebuilder:validation:XValidation:rule="has(self.pvc) == has(oldSelf.pvc)",message="the store block cannot change between pvc and s3"
type SnapshotStorageClassSpec struct {
	// ReclaimPolicy is copied to content bound through this class. An edit
	// applies to content bound afterwards.
	// +kubebuilder:default=Delete
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// PVC selects a claim in the Snapshot namespace that the chart already
	// mounts.
	// +optional
	PVC *PVCStoreSpec `json:"pvc,omitempty"`

	// S3 selects an S3-compatible bucket.
	// +optional
	S3 *S3StoreSpec `json:"s3,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.claimName == oldSelf.claimName && (has(self.basePath) ? self.basePath : '') == (has(oldSelf.basePath) ? oldSelf.basePath : '')",message="claimName and basePath are immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.basePath) || !self.basePath.split('/').exists(p, p == 'artifacts' || p == '..')",message="basePath must not contain an artifacts or .. segment"

// PVCStoreSpec names a claim and a directory in it. An absent and an empty
// basePath compare as equal, so an update that drops the empty field is not
// taken for a location change.
type PVCStoreSpec struct {
	// ClaimName is the claim that holds the snapshots.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ClaimName string `json:"claimName"`

	// BasePath is the directory in the claim that holds the snapshots. The
	// default, "/", is the whole claim.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	BasePath string `json:"basePath,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.endpoint == oldSelf.endpoint && self.region == oldSelf.region && self.bucket == oldSelf.bucket && (has(self.prefix) ? self.prefix : '') == (has(oldSelf.prefix) ? oldSelf.prefix : '')",message="endpoint, region, bucket and prefix are immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.prefix) || !self.prefix.split('/').exists(p, p == 'artifacts' || p == '..')",message="prefix must not contain an artifacts or .. segment"

// S3StoreSpec names a bucket and how to reach it. Endpoint, Region, Bucket and
// Prefix identify the store and are immutable; the rest are access settings
// and stay editable.
type S3StoreSpec struct {
	// Endpoint is the S3-compatible service URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint"`

	// Region is the bucket's region.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Region string `json:"region"`

	// Bucket holds the snapshots.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Bucket string `json:"bucket"`

	// Prefix is the key prefix in the bucket that holds the snapshots.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// CredentialsSecretRef selects a Secret in the Snapshot namespace that
	// holds a shared-credentials file. Omitted, the workload identity of the
	// Snapshot pods is used.
	// +optional
	CredentialsSecretRef *SecretKeyReference `json:"credentialsSecretRef,omitempty"`

	// Addressing selects virtual-hosted or path-style bucket addressing.
	// +kubebuilder:validation:Enum=virtual;path
	// +optional
	Addressing string `json:"addressing,omitempty"`

	// AllowHTTP accepts a plain-http endpoint. For local testing only.
	// +optional
	AllowHTTP bool `json:"allowHttp,omitempty"`

	// CABundleConfigMapRef selects a ConfigMap in the Snapshot namespace that
	// holds the CA bundle for the endpoint.
	// +optional
	CABundleConfigMapRef *ConfigMapKeyReference `json:"caBundleConfigMapRef,omitempty"`
}

// SecretKeyReference names a key of a Secret in the Snapshot namespace.
type SecretKeyReference struct {
	// Name of the Secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key in the Secret's data.
	// +kubebuilder:default=credentials
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Key string `json:"key,omitempty"`
}

// ConfigMapKeyReference names a key of a ConfigMap in the Snapshot namespace.
type ConfigMapKeyReference struct {
	// Name of the ConfigMap.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key in the ConfigMap's data.
	// +kubebuilder:default=ca.crt
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Key string `json:"key,omitempty"`
}

// SnapshotStorageClassStatus is the observed state of a SnapshotStorageClass.
type SnapshotStorageClassStatus struct {
	// StoreID is derived from the store's location fields.
	// +kubebuilder:validation:Pattern=`^store-v1-[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=73
	// +optional
	StoreID string `json:"storeID,omitempty"`

	// Conditions reflect the latest observations of the class. The Ready
	// condition says whether the store can be used.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name must be at most 63 characters"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Ready condition"
// +kubebuilder:printcolumn:name="Reclaim",type="string",JSONPath=".spec.reclaimPolicy",description="Reclaim policy"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SnapshotStorageClass is the cluster-scoped description of one snapshot store.
type SnapshotStorageClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   SnapshotStorageClassSpec   `json:"spec"`
	Status SnapshotStorageClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SnapshotStorageClassList contains a list of SnapshotStorageClass.
type SnapshotStorageClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotStorageClass `json:"items"`
}
