// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"crypto/sha256"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PodSnapshotPhase reports progress without changing the capture conditions.
// +kubebuilder:validation:Enum=Pending;Checkpointing;CheckpointSucceeded;RestoreVerifying;Completed;Failed;VerificationFailed
type PodSnapshotPhase string

const (
	PodSnapshotPhasePending             PodSnapshotPhase = "Pending"
	PodSnapshotPhaseCheckpointing       PodSnapshotPhase = "Checkpointing"
	PodSnapshotPhaseCheckpointSucceeded PodSnapshotPhase = "CheckpointSucceeded"
	PodSnapshotPhaseRestoreVerifying    PodSnapshotPhase = "RestoreVerifying"
	PodSnapshotPhaseCompleted           PodSnapshotPhase = "Completed"
	PodSnapshotPhaseFailed              PodSnapshotPhase = "Failed"
	PodSnapshotPhaseVerificationFailed  PodSnapshotPhase = "VerificationFailed"
)

// RestoreVerificationPodReference pins a caller-created Pod in the snapshot's
// namespace. Create that Pod first, referencing the intended snapshot name.
type RestoreVerificationPodReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Type=string
	UID types.UID `json:"uid"`
}

// RestoreVerificationPolicy is immutable with the containing snapshot spec.
// Every restore destination in the verification Pod must have an application
// readiness probe. Running alone is not a successful application check.
// +kubebuilder:validation:XValidation:rule="self.timeoutSeconds > self.observationWindowSeconds",message="timeoutSeconds must exceed observationWindowSeconds"
type RestoreVerificationPolicy struct {
	PodRef RestoreVerificationPodReference `json:"podRef"`
	// ObservationWindowSeconds requires continuous readiness after restore.
	// Zero skips the timed window, but still requires restore and readiness.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	ObservationWindowSeconds int32 `json:"observationWindowSeconds"`
	// TimeoutSeconds bounds verification, including waiting for the Pod and
	// its destination containers. The clock starts after checkpoint and required
	// SnapshotJob auxiliary work complete, and survives controller restarts.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds int32 `json:"timeoutSeconds"`
}

// RestoreVerificationOutcome is the persisted canary outcome.
// +kubebuilder:validation:Enum=InProgress;Succeeded;Failed
type RestoreVerificationOutcome string

const (
	RestoreVerificationInProgress RestoreVerificationOutcome = "InProgress"
	RestoreVerificationSucceeded  RestoreVerificationOutcome = "Succeeded"
	RestoreVerificationFailed     RestoreVerificationOutcome = "Failed"
)

// RestoreVerificationContainer identifies the runtime execution approved to
// restore. A replacement execution cannot inherit its predecessor's result.
type RestoreVerificationContainer struct {
	Name         string `json:"name"`
	ContainerID  string `json:"containerID"`
	RestartCount int32  `json:"restartCount"`
}

// RestoreVerificationInfo is written by the Operator from observed Pod state,
// not from a caller-provided success marker.
type RestoreVerificationInfo struct {
	ContentUID types.UID                       `json:"contentUID"`
	PolicyHash string                          `json:"policyHash"`
	PodRef     RestoreVerificationPodReference `json:"podRef"`
	// +optional
	// +listType=map
	// +listMapKey=name
	Containers []RestoreVerificationContainer `json:"containers,omitempty"`
	StartedAt  metav1.Time                    `json:"startedAt"`
	DeadlineAt metav1.Time                    `json:"deadlineAt"`
	// HealthySince is reset when readiness is lost during the window.
	// +optional
	HealthySince *metav1.Time `json:"healthySince,omitempty"`
	// +optional
	CompletedAt *metav1.Time               `json:"completedAt,omitempty"`
	Outcome     RestoreVerificationOutcome `json:"outcome"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// RestoreVerificationPolicyHash identifies the exact immutable policy, so a
// result cannot be used with another target or observation requirement.
func RestoreVerificationPolicyHash(p *RestoreVerificationPolicy) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%q %q %d %d",
		p.PodRef.Name, p.PodRef.UID, p.ObservationWindowSeconds, p.TimeoutSeconds))))
}

// IsRestoreVerified requires the persisted success for the current content and
// policy. It does not turn verification-disabled snapshots into verified ones.
func IsRestoreVerified(s *PodSnapshot, contentUID types.UID) bool {
	i, p := s.Status.RestoreVerifyInfo, s.Spec.RestoreVerification
	return p != nil && i != nil && contentUID != "" &&
		IsPodSnapshotSucceeded(s) && !IsPodSnapshotFailed(s) &&
		s.Status.Phase == PodSnapshotPhaseCompleted &&
		i.Outcome == RestoreVerificationSucceeded && i.CompletedAt != nil &&
		i.ContentUID == contentUID && i.PodRef == p.PodRef &&
		i.PolicyHash == RestoreVerificationPolicyHash(p) && len(i.Containers) > 0
}
