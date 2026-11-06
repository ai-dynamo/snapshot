// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import "k8s.io/apimachinery/pkg/types"

// PodGroupSnapshot and PodGroupRestore condition types. Only the operator sets
// them, via meta.SetStatusCondition.
const (
	// PodGroupConditionAdmitted is True once every member has passed admission,
	// which commits the attempt.
	PodGroupConditionAdmitted = "Admitted"
	// PodGroupConditionReleased is True once a PodGroupRestore's attempt has
	// reached release, from which point its released members run whatever
	// happens to their peers. It applies to PodGroupRestore only.
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
	// PodGroupReasonGateBudgetTooShort means a restore target's startup probe
	// cannot wait for what remains of the deadline.
	PodGroupReasonGateBudgetTooShort = "GateBudgetTooShort"
	// PodGroupReasonPodIPChanged means a restore Pod's IP or admitted container
	// changed after the operator pinned it.
	PodGroupReasonPodIPChanged = "PodIPChanged"
	// PodGroupReasonTargetInUse means the target is bound, by name and UID, to
	// another unfinished PodGroupRestore, or that another unfinished one has
	// already pinned the target's UID.
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
	// target's Pod: it does not exist yet, is not yet bound to the attempt by
	// name and UID, is not scheduled to a node, has no IP address yet, or its
	// destination container has no ID yet. The condition's message names each
	// target and what it waits for.
	PodGroupReasonTargetPodPending = "TargetPodPending"
	// PodGroupReasonPreviousGroupCleanup means admission is waiting for a
	// PodSnapshot at a member's name to be deleted: it belongs to an earlier
	// PodGroupSnapshot of the same name, whose PodSnapshots garbage collection
	// is removing. The condition's message names each one.
	PodGroupReasonPreviousGroupCleanup = "PreviousGroupCleanup"
	// PodGroupReasonAdmitting means the operator has started creating the
	// members' work, and the attempt is waiting for every member to pass
	// admission.
	PodGroupReasonAdmitting = "Admitting"
	// PodGroupReasonCommitted means every member passed admission, so the
	// attempt is committed: a member's agent has started changing its source.
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
	Name string `json:"name"`

	// UID of the object.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid"`
}
