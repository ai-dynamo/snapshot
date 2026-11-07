// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PodGroupRestoreSpec defines the desired state of PodGroupRestore: the
// PodGroupSnapshot to restore, and the Pod that receives each of its members.
// The API server checks its shape. The operator checks the rest before it
// admits the restore: that the targets match the snapshot's members one to
// one and name distinct Pods, and that each target Pod exists and is bound to
// this PodGroupRestore. The node agents then check that each target's startup
// probe allows for what remains of the deadline.
type PodGroupRestoreSpec struct {
	// SnapshotRef names the PodGroupSnapshot to restore, in this namespace.
	// Its UID is required, so a recreated PodGroupSnapshot with the same name
	// is never restored in its place.
	// +kubebuilder:validation:Required
	SnapshotRef ObjectIdentity `json:"snapshotRef"`

	// DeadlineSeconds bounds the whole restore, counted from the
	// PodGroupRestore's creation, and is at most a day. Members still restoring
	// when it expires fail with DeadlineExceeded; members already released keep
	// running.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	DeadlineSeconds int64 `json:"deadlineSeconds"`

	// Targets are the restore Pods, one for each member of the
	// PodGroupSnapshot. Their order carries no meaning.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=id
	Targets []PodGroupRestoreTarget `json:"targets"`
}

// PodGroupRestoreTarget is the restore Pod for one member of the
// PodGroupSnapshot. A target's rank is the rank of the snapshot member with the
// same ID; the restore never restates it.
type PodGroupRestoreTarget struct {
	// ID is the ID of the PodGroupSnapshot member that this target restores.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ID string `json:"id"`

	// PodRef names the restore Pod, in this namespace.
	// +kubebuilder:validation:Required
	PodRef TargetPodReference `json:"podRef"`

	// ContainerMappings maps the member's checkpointed container to the
	// restore Pod's container that receives its processes. When it is
	// omitted, the checkpointed container restores into the container of the
	// same name. It holds one entry, as PodReference.Containers does.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	// +listType=map
	// +listMapKey=source
	ContainerMappings []ContainerMapping `json:"containerMappings,omitempty"`
}

// TargetPodReference names a restore Pod, which may not exist yet when the
// PodGroupRestore is created.
type TargetPodReference struct {
	// Name of the restore Pod.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// UID of the restore Pod. When it is omitted, the operator admits the Pod
	// that has this name at admission time, and records its UID in status.
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid,omitempty"`
}

// ContainerMapping maps a member's checkpointed container to the restore Pod's
// container that receives its processes.
type ContainerMapping struct {
	// Source is the member's checkpointed container.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Source string `json:"source"`

	// Destination is the restore Pod's container that receives the processes.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Destination string `json:"destination"`
}

// RestorePhase is a member's progress through a PodGroupRestore. Released and
// Failed are final.
// +kubebuilder:validation:Enum=Pending;Admitted;Restoring;Released;Failed
type RestorePhase string

const (
	// RestorePhasePending means the member has not yet passed admission.
	RestorePhasePending RestorePhase = "Pending"
	// RestorePhaseAdmitted means the member passed admission and its bindings
	// are pinned. The restore commits once every member has.
	RestorePhaseAdmitted RestorePhase = "Admitted"
	// RestorePhaseRestoring means the member's processes are being restored.
	RestorePhaseRestoring RestorePhase = "Restoring"
	// RestorePhaseReleased means the member's processes were restored and
	// released to run.
	RestorePhaseReleased RestorePhase = "Released"
	// RestorePhaseFailed means the member's restore failed; the member's reason
	// says why.
	RestorePhaseFailed RestorePhase = "Failed"
)

// PodGroupRestoreMemberStatus is the observed state of one target. Its
// bindings and startedAt are write-once, and its phase stops changing once
// final.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.pod) || (has(self.pod) && self.pod == oldSelf.pod)",message="pod is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.nodeName) || (has(self.nodeName) && self.nodeName == oldSelf.nodeName)",message="nodeName is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.containers) || (has(self.containers) && self.containers == oldSelf.containers)",message="containers is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.targetPodIP) || (has(self.targetPodIP) && self.targetPodIP == oldSelf.targetPodIP)",message="targetPodIP is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.startedAt) || (has(self.startedAt) && self.startedAt == oldSelf.startedAt)",message="startedAt is write-once"
// +kubebuilder:validation:XValidation:rule="!(oldSelf.phase in ['Released', 'Failed']) || self.phase == oldSelf.phase",message="Released and Failed are final phases"
type PodGroupRestoreMemberStatus struct {
	// ID is the target's ID in spec.targets.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ID string `json:"id"`

	// Pod is the restore Pod, pinned when the operator admits the target.
	// +optional
	Pod *ObjectIdentity `json:"pod,omitempty"`

	// NodeName is the node the restore Pod runs on, pinned at admission.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	NodeName string `json:"nodeName,omitempty"`

	// Containers are the restore Pod's destination containers, each with the
	// runtime ID it had at admission, when the operator pinned it. It holds
	// one entry, as the container mappings do.
	// +optional
	// +kubebuilder:validation:MaxItems=1
	// +listType=map
	// +listMapKey=name
	Containers []PodGroupRestoreContainerStatus `json:"containers,omitempty"`

	// TargetPodIP is the restore Pod's IPv4 address, pinned at admission.
	// +optional
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Format=ipv4
	TargetPodIP string `json:"targetPodIP,omitempty"`

	// StartedAt is when Snapshot began restoring into the restore Pod: the
	// time of the restore Pod's nvidia.com/RestoreStarted condition, which the
	// node agent sets before its first irreversible step. Absent means
	// Snapshot never changed the restore Pod.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// Phase is the member's progress through the restore.
	// +optional
	// +kubebuilder:default=Pending
	Phase RestorePhase `json:"phase,omitempty"`

	// Reason is a CamelCase word that explains the phase, such as
	// PodIPChanged for a Failed member.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:Pattern=`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`
	Reason string `json:"reason,omitempty"`

	// Message is a human-readable explanation of the phase, in at most 1024
	// characters, so that the messages of 256 members fit in one object.
	// Longer explanations belong in the group's conditions.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}

// PodGroupRestoreContainerStatus pins one destination container of a restore
// Pod.
type PodGroupRestoreContainerStatus struct {
	// Name of the destination container.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// ContainerID is the container's runtime ID at admission.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ContainerID string `json:"containerID"`
}

// PodGroupRestoreStatus defines the observed state of PodGroupRestore. Only the
// operator writes it.
type PodGroupRestoreStatus struct {
	// Members reports each target's progress, keyed by ID. An entry names a
	// target of spec.targets and, once written, is never removed.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=id
	Members []PodGroupRestoreMemberStatus `json:"members,omitempty"`

	// Conditions reflect the latest observations of the PodGroupRestore's
	// state. Types: Admitted, Released, Ready, Failed.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=podgrouprestore
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.members) || self.status.members.all(m, self.spec.targets.exists(t, t.id == m.id))",message="status.members must name targets of spec.targets"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.members) || (has(self.status) && has(self.status.members) && oldSelf.status.members.all(o, self.status.members.exists(n, n.id == o.id)))",message="status.members entries cannot be removed"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Ready condition"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Failed')].reason",description="Failure reason"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PodGroupRestore restores every member of a PodGroupSnapshot into its own
// restore Pod, as one attempt. A PodGroupSnapshot can be restored many times;
// each PodGroupRestore is a new attempt with new restore Pods.
//
// No conversion: this type exists only in v1alpha1 (no other API version), so
// it is not part of any conversion scheme.
type PodGroupRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec   PodGroupRestoreSpec   `json:"spec"`
	Status PodGroupRestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodGroupRestoreList contains a list of PodGroupRestore.
type PodGroupRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodGroupRestore `json:"items"`
}
