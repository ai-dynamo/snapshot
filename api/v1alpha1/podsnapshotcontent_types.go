// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// IsPodSnapshotContentSucceeded reports whether the PodSnapshotContent's Ready condition is True.
func IsPodSnapshotContentSucceeded(c *PodSnapshotContent) bool {
	return meta.IsStatusConditionTrue(c.Status.Conditions, PodSnapshotConditionReady)
}

// IsPodSnapshotContentFailed reports whether the PodSnapshotContent's Failed condition is True.
func IsPodSnapshotContentFailed(c *PodSnapshotContent) bool {
	return meta.IsStatusConditionTrue(c.Status.Conditions, PodSnapshotConditionFailed)
}

// PodSnapshotContentSpec defines the desired state of PodSnapshotContent. It is
// populated by the PodSnapshotReconciler (operator) at creation time and is
// immutable thereafter.
// +kubebuilder:validation:XValidation:rule="!has(self.containers) || self.containers.all(c, c.name in (has(self.source.artifact) ? self.source.artifact.containers : self.source.podRef.containers))",message="containers must name containers in source.podRef.containers or source.artifact.containers"
type PodSnapshotContentSpec struct {
	// PodSnapshotRef is the back-pointer to the bound PodSnapshot. It may span
	// namespaces because PodSnapshotContent is cluster-scoped.
	// +kubebuilder:validation:Required
	PodSnapshotRef PodSnapshotReference `json:"snapshotRef"`

	// Source describes what to checkpoint — the source pod and the node it runs
	// on — or, for an import, the existing artifact the content stands for.
	// +kubebuilder:validation:Required
	Source PodSnapshotContentSource `json:"source"`

	// Storage binds this content to one configured store. Omitted on legacy
	// content; the producer will start writing it with artifact-flow integration.
	// Like the rest of spec it cannot be added, removed or changed after creation.
	// +optional
	Storage *CheckpointStorageBinding `json:"storage,omitempty"`

	// Containers is the PodSnapshot's container settings, with defaults
	// applied, saved at creation. Every restore of this snapshot uses them,
	// even if the PodSnapshot or SnapshotJob that asked for it changes or is
	// gone.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=1
	Containers []ContainerSnapshotConfig `json:"containers,omitempty"`
}

// PodSnapshotReference is a cross-namespace reference to a PodSnapshot.
type PodSnapshotReference struct {
	// Namespace of the referenced PodSnapshot.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Name of the referenced PodSnapshot.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// UID of the referenced PodSnapshot, recorded at binding time to detect a
	// stale reference after a delete and recreate.
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// PodSnapshotContentSource is the immutable source descriptor: a checkpoint —
// what to dump (PodRef) and where it runs (NodeName) — or an imported Artifact.
// +kubebuilder:validation:XValidation:rule="has(self.artifact) ? !has(self.podRef) && !has(self.nodeName) : has(self.podRef) && has(self.nodeName)",message="a checkpoint needs podRef and nodeName; an import needs artifact and neither"
type PodSnapshotContentSource struct {
	// PodRef identifies the pod to dump. Its UID guards against dumping a
	// same-named recreation of the pod.
	// +optional
	PodRef *PodReference `json:"podRef,omitempty"`

	// NodeName is the node the source pod runs on, denormalized from the live
	// pod so it travels with PodRef as one immutable unit and selects the node
	// agent that performs the dump.
	// +optional
	// +kubebuilder:validation:MinLength=1
	NodeName string `json:"nodeName,omitempty"`

	// Artifact is the existing artifact an imported content stands for, copied
	// from its PodSnapshot. No agent checkpoints for it: it has no node.
	// +optional
	Artifact *ArtifactReference `json:"artifact,omitempty"`
}

// IsImported reports whether the content stands for an existing artifact
// rather than a checkpoint it asks for.
func (c *PodSnapshotContent) IsImported() bool {
	return c.Spec.Source.Artifact != nil
}

// ArtifactUID is the identity the content's artifact is stored and recorded
// under: the content's own UID for a checkpoint taken here, and for an import
// the UID it was taken under elsewhere. Readers derive the artifact's location and check
// its manifest with this, never with metadata.uid directly.
func (c *PodSnapshotContent) ArtifactUID() types.UID {
	if c.Spec.Source.Artifact != nil {
		return c.Spec.Source.Artifact.UID
	}
	return c.UID
}

// TargetContainers returns the checkpointed container names, whichever source is set.
func (c *PodSnapshotContent) TargetContainers() []string {
	switch {
	case c.Spec.Source.Artifact != nil:
		return c.Spec.Source.Artifact.Containers
	case c.Spec.Source.PodRef != nil:
		return c.Spec.Source.PodRef.Containers
	default:
		return nil
	}
}

// PodSnapshotContentStatus defines the observed state of PodSnapshotContent.
type PodSnapshotContentStatus struct {
	// Conditions reflect the latest observations of the PodSnapshotContent's state.
	// Standard types are Ready and Failed.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Source publishes where the snapshot was taken, written when the content
	// goes Ready.
	// +optional
	Source *CheckpointSource `json:"source,omitempty"`

	// Storage records confirmed per-container publications. Existing filesystem
	// callers leave this absent; its presence does not replace Ready/Failed.
	// +optional
	Storage *CheckpointStorageStatus `json:"storage,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=podsnapcontent
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.storage) || has(self.spec.storage)",message="publication descriptors require a storage binding"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.storage) || self.status.storage.artifacts.all(a, (has(self.spec.source.artifact) ? self.spec.source.artifact.containers : self.spec.source.podRef.containers).exists(c, c == a.containerName))",message="publication descriptors must name checkpointed containers"
// +kubebuilder:printcolumn:name="PodSnapshot",type="string",JSONPath=".spec.snapshotRef.name",description="Bound PodSnapshot"
// +kubebuilder:printcolumn:name="Namespace",type="string",JSONPath=".spec.snapshotRef.namespace",description="PodSnapshot namespace"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Ready condition"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PodSnapshotContent is the Schema for the snapshotcontents API. It is the
// cluster-scoped record of one container's snapshot.
//
// No conversion: this type exists only in v1alpha1 (no other API version), so it
// is not part of any conversion scheme.
type PodSnapshotContent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec   PodSnapshotContentSpec   `json:"spec,omitempty"`
	Status PodSnapshotContentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodSnapshotContentList contains a list of PodSnapshotContent.
type PodSnapshotContentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodSnapshotContent `json:"items"`
}
