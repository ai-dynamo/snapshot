// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "k8s.io/apimachinery/pkg/types"

// PodGroupMaxDeadlineSeconds is the longest deadlineSeconds of a
// PodGroupSnapshot or PodGroupRestore: the Maximum marker on both fields. A
// group restore target's startup probe allows one more one-second failure,
// since its first check can run immediately.
const PodGroupMaxDeadlineSeconds = 86400

// PodGroupSnapshot and PodGroupRestore condition types. Only the operator sets
// them, via meta.SetStatusCondition.
const (
	// PodGroupConditionAdmitted is True, with reason Committed, once the
	// attempt is committed: the agents commit it at their admitted barrier, and
	// the operator sees that as the first member's startedAt. Before that, it is
	// False with reason Admitting while the members' work is created and
	// admitted, and False with the wait's or the failure's reason otherwise.
	PodGroupConditionAdmitted = "Admitted"
	// PodGroupConditionReleased turns True on a PodGroupRestore's first
	// Released member, which proves that the agents' release barrier
	// completed. A member's release point is its restore-complete, and the
	// member is Released once any postRestore notification has also succeeded.
	// Released members keep running whatever happens to their peers. It
	// applies to PodGroupRestore only.
	PodGroupConditionReleased = "Released"
	// PodGroupConditionReady is True when every member of a PodGroupSnapshot is
	// Checkpointed, or every member of a PodGroupRestore is Released.
	PodGroupConditionReady = "Ready"
	// PodGroupConditionFailed is True when the attempt failed terminally.
	PodGroupConditionFailed = "Failed"
)

// Reasons on PodGroupSnapshot and PodGroupRestore conditions and member statuses.
const (
	// PodGroupReasonRefused means the attempt ended before Snapshot changed any
	// member, for example because admission refused a member.
	PodGroupReasonRefused = "Refused"
	// PodGroupReasonDeadlineExceeded means spec.deadlineSeconds expired.
	PodGroupReasonDeadlineExceeded = "DeadlineExceeded"
	// PodGroupReasonPodIPChanged means a restore Pod's IP or admitted container
	// changed after the operator pinned it.
	PodGroupReasonPodIPChanged = "PodIPChanged"
	// PodGroupReasonTargetInUse means the target Pod is already claimed by
	// another PodGroupRestore.
	PodGroupReasonTargetInUse = "TargetInUse"
	// PodGroupReasonTargetPodDeleted fails a restore member whose pinned Pod is
	// gone or was replaced. Like a deleted member PodSnapshot, it may hide the
	// member's start, since the Pod's nvidia.com/RestoreStarted condition went
	// with it.
	PodGroupReasonTargetPodDeleted = "TargetPodDeleted"
	// PodGroupReasonArtifactUnavailable means a member's snapshot cannot be
	// restored.
	PodGroupReasonArtifactUnavailable = "ArtifactUnavailable"
	// PodGroupReasonMemberFailed means a member failed after admission, which
	// failed the attempt.
	PodGroupReasonMemberFailed = "MemberFailed"
	// PodGroupReasonCoordinatorLost means the attempt lost its coordination
	// session, for example because the coordinator restarted.
	PodGroupReasonCoordinatorLost = "CoordinatorLost"
	// PodGroupReasonCancelled means the attempt was cancelled because its group
	// was deleted. A group reports it only when no member started; a member can
	// report it after starting, when the deletion raced the commit.
	PodGroupReasonCancelled = "Cancelled"
	// PodGroupReasonAgentNotReady means admission is waiting for the node agent
	// of a member's node: no agent Pod runs there, it is not Ready, or it does
	// not support a compatible group protocol. The condition's message names
	// each node and why. The attempt waits until the agent is ready or the
	// deadline expires.
	PodGroupReasonAgentNotReady = "AgentNotReady"
	// PodGroupReasonSourcePodUnscheduled means admission is waiting for a
	// member's source Pod to be scheduled to a node.
	PodGroupReasonSourcePodUnscheduled = "SourcePodUnscheduled"
	// PodGroupReasonTargetPodPending means admission is waiting for a restore
	// target's Pod: it does not exist yet, or, once claimed, has no IP address
	// or destination container ID yet. The condition's message names each
	// target and what it waits for.
	PodGroupReasonTargetPodPending = "TargetPodPending"
	// PodGroupReasonTargetPodUnscheduled means admission is waiting for a
	// claimed restore target's Pod to be scheduled to a node, typically for
	// lack of capacity. The condition's message names each target.
	PodGroupReasonTargetPodUnscheduled = "TargetPodUnscheduled"
	// PodGroupReasonPreviousGroupCleanup means admission is waiting for a
	// PodSnapshot at a member's name to be deleted: it belongs to an earlier
	// PodGroupSnapshot of the same name, whose PodSnapshots garbage collection
	// is removing. The condition's message names each one.
	PodGroupReasonPreviousGroupCleanup = "PreviousGroupCleanup"
	// PodGroupReasonAdmitting means the operator has started creating the
	// members' work, and the attempt is waiting for the agents to pass their
	// admitted barrier.
	PodGroupReasonAdmitting = "Admitting"
	// PodGroupReasonCommitted means the agents passed their admitted barrier,
	// so the attempt is committed: a member's agent has started changing its
	// source.
	PodGroupReasonCommitted = "Committed"
)

// ObjectIdentity names one exact object: the object with this name and UID,
// never a later object that reuses the name. A namespaced object is in the
// namespace of the object that refers to it.
type ObjectIdentity struct {
	// Name of the object.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// UID of the object.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid"`
}

// PodGroupSnapshotMembership identifies one member of a coordinated checkpoint.
// Its name and UID identify the PodGroupSnapshot. The PodGroupSnapshot
// controller sets it on the member's PodSnapshot, and the PodSnapshot
// controller copies it unchanged into the PodSnapshotContent.
// A content with this membership is checkpointed with its group, never alone.
// The group is in the PodSnapshot's namespace, or spec.snapshotRef.namespace
// on a cluster-scoped PodSnapshotContent.
type PodGroupSnapshotMembership struct {
	// ObjectIdentity names the PodGroupSnapshot.
	ObjectIdentity `json:",inline"`

	// MemberID identifies the member in the PodGroupSnapshot's spec.members.
	// Its rank is that member's position in the list.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	MemberID string `json:"memberID"`
}
