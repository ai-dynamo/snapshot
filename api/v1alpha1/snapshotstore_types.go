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

// SnapshotStoreSpec describes one snapshot store. Set exactly one of pvc or
// s3.
// +kubebuilder:validation:XValidation:rule="has(self.pvc) != has(self.s3)",message="exactly one of pvc or s3 must be set"
// +kubebuilder:validation:XValidation:rule="has(self.pvc) == has(oldSelf.pvc)",message="the store block cannot change between pvc and s3"
type SnapshotStoreSpec struct {
	// ReclaimPolicy is copied to content bound to this store. A change
	// affects content bound afterwards.
	// +kubebuilder:default=Delete
	// +optional
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// PVC stores snapshots in a claim in the Snapshot namespace.
	// +optional
	PVC *PVCStoreSpec `json:"pvc,omitempty"`

	// S3 stores snapshots in an S3-compatible bucket.
	// +optional
	S3 *S3StoreSpec `json:"s3,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.claimName == oldSelf.claimName && self.subPath == oldSelf.subPath",message="claimName and subPath are immutable"
// +kubebuilder:validation:XValidation:rule="!self.subPath.split('/').exists(p, p == 'artifacts' || p == '.claims' || p == '..')",message="subPath must not contain an artifacts, .claims or .. segment"

// PVCStoreSpec is a claim and a directory in it. The claim must be mounted
// into the operator and every node agent.
type PVCStoreSpec struct {
	// ClaimName is the claim that holds the snapshots.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ClaimName string `json:"claimName"`

	// SubPath is the directory in the claim that holds the snapshots. The
	// default, "/", is the whole claim.
	// +kubebuilder:default="/"
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:Pattern=`^/`
	// +optional
	SubPath string `json:"subPath,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.endpoint == oldSelf.endpoint && self.region == oldSelf.region && self.bucket == oldSelf.bucket && (has(self.prefix) ? self.prefix : '') == (has(oldSelf.prefix) ? oldSelf.prefix : '')",message="endpoint, region, bucket and prefix are immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.prefix) || !self.prefix.split('/').exists(p, p == 'artifacts' || p == '.claims' || p == '..')",message="prefix must not contain an artifacts, .claims or .. segment"
// +kubebuilder:validation:XValidation:rule="(has(self.allowHTTP) ? self.allowHTTP : false) || !self.endpoint.startsWith('http://')",message="allowHTTP is required for an http:// endpoint"

// S3StoreSpec is a bucket and how to reach it. Endpoint, region, bucket and
// prefix identify the store and cannot change; the other fields can. An
// absent and an empty prefix are the same.
type S3StoreSpec struct {
	// Endpoint is the S3-compatible service URL: an absolute http(s) URL with
	// no path, query, fragment or userinfo.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://[^/?#@]+$`
	Endpoint string `json:"endpoint"`

	// Region is the bucket's region.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Region string `json:"region"`

	// Bucket holds the snapshots. It is a DNS-style name: lowercase letters,
	// digits, dots and hyphens.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`
	Bucket string `json:"bucket"`

	// Prefix is the key prefix in the bucket that holds the snapshots. It is
	// relative; empty is the bucket root.
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[^/].*$|^$`
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// CredentialsSecretRef is a Secret in the Snapshot namespace that holds a
	// shared-credentials file. Without it, the workload identity of the
	// Snapshot pods is used.
	// +optional
	CredentialsSecretRef *SecretKeyReference `json:"credentialsSecretRef,omitempty"`

	// Addressing selects virtual-hosted or path-style bucket addressing.
	// +kubebuilder:validation:Enum=virtual;path
	// +optional
	Addressing string `json:"addressing,omitempty"`

	// AllowHTTP accepts a plain-http endpoint. It is required for an http://
	// endpoint. For local testing only.
	// +optional
	AllowHTTP bool `json:"allowHTTP,omitempty"`

	// CABundleConfigMapRef is a ConfigMap in the Snapshot namespace that holds
	// the CA bundle for the endpoint.
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

// SnapshotStoreStatus is the observed state of a SnapshotStore.
type SnapshotStoreStatus struct {
	// StoreID identifies the store. It is derived from the location fields.
	// +kubebuilder:validation:Pattern=`^store-v1-[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=73
	// +optional
	StoreID string `json:"storeID,omitempty"`

	// Conditions report the state of the store. Ready says whether the store
	// can be used.
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

// SnapshotStore is the cluster-scoped description of one snapshot store.
type SnapshotStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	Spec   SnapshotStoreSpec   `json:"spec"`
	Status SnapshotStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SnapshotStoreList contains a list of SnapshotStore.
type SnapshotStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotStore `json:"items"`
}
