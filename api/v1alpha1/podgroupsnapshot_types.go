// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PodGroupSnapshotSpec defines the desired state of PodGroupSnapshot: the Pods
// that are checkpointed together, in rank order. The API server checks its
// shape. The operator checks the rest before admission: that the members name
// distinct Pods, and that each source Pod exists with the named UID.
// +kubebuilder:validation:XValidation:rule="self.members.all(m, self.members.exists_one(n, n.id == m.id))",message="member IDs must be unique"
type PodGroupSnapshotSpec struct {
	// DeadlineSeconds bounds the whole checkpoint, counted from the
	// PodGroupSnapshot's creation, and is at most a day. Members still being
	// checkpointed when it expires fail with DeadlineExceeded; members already
	// checkpointed keep their snapshots.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	DeadlineSeconds int64 `json:"deadlineSeconds"`

	// Members are the Pods to checkpoint together, 1 to 256 of them. A
	// member's position in the list is its rank, which every restore of the
	// group reuses, so the list is ordered: like the rest of the spec, the
	// order cannot change.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Members []PodGroupSnapshotMember `json:"members"`
}

// PodGroupSnapshotMember is one Pod of a PodGroupSnapshot.
// +kubebuilder:validation:XValidation:rule="!has(self.podSnapshotTemplate) || !has(self.podSnapshotTemplate.containers) || self.podSnapshotTemplate.containers.all(c, c.name in self.source.podRef.containers)",message="podSnapshotTemplate.containers must name containers in source.podRef.containers"
type PodGroupSnapshotMember struct {
	// ID names the member from checkpoint to restore: the PodGroupRestore
	// target with the same ID restores this member. It must be a DNS label.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ID string `json:"id"`

	// Source identifies the Pod to checkpoint, as for a standalone
	// PodSnapshot, except that the Pod's UID is required, so a recreated Pod
	// with the same name is never checkpointed in its place.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="has(self.podRef.uid) && size(self.podRef.uid) > 0",message="member sources must name the Pod UID"
	Source PodSnapshotSource `json:"source"`

	// PodSnapshotTemplate holds the settings that this member's PodSnapshot
	// receives. A member without one gets the default settings of a
	// PodSnapshot. Settings never change the member's rank or its part in the
	// group's checkpoint and restore.
	// +optional
	PodSnapshotTemplate *PodGroupMemberTemplate `json:"podSnapshotTemplate,omitempty"`
}

// PodGroupMemberTemplate holds the settings of one member's PodSnapshot:
// labels and annotations, and the settings of the checkpointed container,
// which say when the member is safe to checkpoint and how its restored
// application learns that it may resume. The settings are copied to the
// member's PodSnapshot, and from there into its PodSnapshotContent, so every
// restore of the member uses them.
//
// The controller owns everything else on that PodSnapshot: its name, owner
// reference and group labels, and spec.source, which it takes from the member.
// This is a type of its own rather than SnapshotJob's PodSnapshotTemplate
// because a member's source already names its containers.
// +kubebuilder:validation:XValidation:rule="!has(self.metadata) || !has(self.metadata.labels) || !('nvidia.com/snapshot-podgroup' in self.metadata.labels)",message="metadata.labels must not set controller-owned label nvidia.com/snapshot-podgroup"
// +kubebuilder:validation:XValidation:rule="!has(self.metadata) || !has(self.metadata.labels) || !('nvidia.com/snapshot-podgroup-member' in self.metadata.labels)",message="metadata.labels must not set controller-owned label nvidia.com/snapshot-podgroup-member"
type PodGroupMemberTemplate struct {
	// Metadata holds labels and annotations copied to the member's
	// PodSnapshot. The group's own labels are reserved for the controller.
	// +optional
	Metadata *PodSnapshotTemplateMetadata `json:"metadata,omitempty"`

	// Containers holds the settings of the member's checkpointed container,
	// matched by name to source.podRef.containers.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=1
	Containers []ContainerSnapshotConfig `json:"containers,omitempty"`
}

// CheckpointPhase is a member's progress through the checkpoint of its
// PodGroupSnapshot. Checkpointed and Failed are final.
// +kubebuilder:validation:Enum=Pending;Admitted;Checkpointing;Checkpointed;Failed
type CheckpointPhase string

const (
	// CheckpointPhasePending means the member has not yet passed admission.
	CheckpointPhasePending CheckpointPhase = "Pending"
	// CheckpointPhaseAdmitted means the member passed admission. The checkpoint
	// commits once every member has.
	CheckpointPhaseAdmitted CheckpointPhase = "Admitted"
	// CheckpointPhaseCheckpointing means the member is being checkpointed.
	CheckpointPhaseCheckpointing CheckpointPhase = "Checkpointing"
	// CheckpointPhaseCheckpointed means the member's snapshot is complete.
	CheckpointPhaseCheckpointed CheckpointPhase = "Checkpointed"
	// CheckpointPhaseFailed means checkpointing the member failed; the member's
	// reason says why.
	CheckpointPhaseFailed CheckpointPhase = "Failed"
)

// PodGroupSnapshotMemberStatus is the observed state of one member. Its
// bindings and startedAt are write-once, and its phase stops changing once
// final.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.podSnapshot) || (has(self.podSnapshot) && self.podSnapshot == oldSelf.podSnapshot)",message="podSnapshot is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.content) || (has(self.content) && self.content == oldSelf.content)",message="content is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.startedAt) || (has(self.startedAt) && self.startedAt == oldSelf.startedAt)",message="startedAt is write-once"
// +kubebuilder:validation:XValidation:rule="!(oldSelf.phase in ['Checkpointed', 'Failed']) || self.phase == oldSelf.phase",message="Checkpointed and Failed are final phases"
type PodGroupSnapshotMemberStatus struct {
	// ID is the member's ID in spec.members.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ID string `json:"id"`

	// Rank is the member's position in spec.members.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	Rank int32 `json:"rank"`

	// NodeName is the node the source Pod runs on.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	NodeName string `json:"nodeName,omitempty"`

	// PodSnapshot is the PodSnapshot that the PodGroupSnapshot owns for this
	// member.
	// +optional
	PodSnapshot *ObjectIdentity `json:"podSnapshot,omitempty"`

	// Content is the member's PodSnapshotContent, which records its snapshot.
	// +optional
	Content *ObjectIdentity `json:"content,omitempty"`

	// StartedAt is when Snapshot began changing the source Pod: the time of
	// the Started condition that the node agent sets on the member's
	// PodSnapshotContent before its first irreversible step. Absent means
	// Snapshot never changed the source Pod.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// Phase is the member's progress through the checkpoint.
	// +optional
	// +kubebuilder:default=Pending
	Phase CheckpointPhase `json:"phase,omitempty"`

	// Reason is a CamelCase word that explains the phase, such as
	// DeadlineExceeded for a Failed member.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable explanation of the phase.
	// +optional
	// +kubebuilder:validation:MaxLength=32768
	Message string `json:"message,omitempty"`
}

// PodGroupSnapshotStatus defines the observed state of PodGroupSnapshot. Only
// the operator writes it.
type PodGroupSnapshotStatus struct {
	// Members reports each member's progress, keyed by ID. An entry names a
	// member of spec.members and, once written, is never removed.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=id
	Members []PodGroupSnapshotMemberStatus `json:"members,omitempty"`

	// Conditions reflect the latest observations of the PodGroupSnapshot's
	// state. Types: Admitted, Ready, Failed.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=podgroupsnap
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be no more than 63 characters because member PodSnapshots carry it as a label value"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.members) || self.status.members.all(m, self.spec.members.exists(s, s.id == m.id))",message="status.members must name members of spec.members"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.members) || (has(self.status) && has(self.status.members) && oldSelf.status.members.all(o, self.status.members.exists(n, n.id == o.id)))",message="status.members entries cannot be removed"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Ready condition"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Failed')].reason",description="Failure reason"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PodGroupSnapshot checkpoints a fixed group of cooperating Pods together, so
// that a PodGroupRestore can restore them together. It owns one PodSnapshot
// per member, so each member's snapshot is stored and cleaned up like a
// standalone one.
//
// No conversion: this type exists only in v1alpha1 (no other API version), so
// it is not part of any conversion scheme.
type PodGroupSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec   PodGroupSnapshotSpec   `json:"spec"`
	Status PodGroupSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodGroupSnapshotList contains a list of PodGroupSnapshot.
type PodGroupSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodGroupSnapshot `json:"items"`
}
