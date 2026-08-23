// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SnapshotJob condition types. The controller sets these via meta.SetStatusCondition.
const (
	// SnapshotJobConditionRunning is True when the source pod is running and ready
	// (readiness probe passed).
	SnapshotJobConditionRunning = "Running"
	// SnapshotJobConditionCaptured is True when the CRIU dump of the target
	// container is complete (PodSnapshot Ready=True).
	SnapshotJobConditionCaptured = "Captured"
	// SnapshotJobConditionCompleted is True when the checkpoint is captured and
	// the source batch/v1 Job has completed its post-capture workload logic.
	SnapshotJobConditionCompleted = "Completed"
	// SnapshotJobConditionFailed is True on terminal failure. The batch/v1 Job is
	// preserved for status and debugging; Kubernetes controls failed pod retention.
	SnapshotJobConditionFailed = "Failed"
)

// SnapshotJob condition reasons.
const (
	// Running
	ReasonPodPending = "PodPending"
	ReasonPodReady   = "PodReady"

	// Captured
	ReasonCaptureInProgress = "CaptureInProgress"
	ReasonCaptureCompleted  = "CaptureCompleted"

	// Completed
	ReasonWaitingForPodCompletion = "WaitingForPodCompletion"
	ReasonJobCompleted            = "JobCompleted"

	// Failed=True
	ReasonCaptureFailed     = "CaptureFailed"
	ReasonPodSnapshotFailed = "PodSnapshotFailed"
	ReasonJobFailed         = "JobFailed"
	ReasonDeadlineExceeded  = "DeadlineExceeded"
	// ReasonSourceCompletedWithoutCapture means the BatchJob reported Complete=True,
	// but the authoritative PodSnapshotContent remained nonterminal after the source
	// pod exited. There is no observed capture error; the required capture result is
	// missing after the source exited.
	ReasonSourceCompletedWithoutCapture = "SourceCompletedWithoutCapture"
	ReasonPodSnapshotNameConflict       = "PodSnapshotNameConflict"
	ReasonJobNameConflict               = "JobNameConflict"
	ReasonJobDeleted                    = "JobDeleted"
	// ReasonInvalidSpec covers spec-level validation failures caught before
	// building the source Job, for objects that bypassed CEL admission.
	ReasonInvalidSpec = "InvalidSpec"
)

// SnapshotJobOwnerLabel maps a produced PodSnapshot back to the SnapshotJob that
// created it. The PodSnapshot deliberately carries no ownerReference (artifacts
// must outlive the SnapshotJob), so this label is the lookup key controllers use
// instead of an owner-based watch.
const SnapshotJobOwnerLabel = "nvidia.com/snapshot-job"

// SnapshotJobOwnerUIDLabel binds a produced PodSnapshot to one concrete
// SnapshotJob incarnation. The name label alone is insufficient because capture
// artifacts outlive their SnapshotJob and names can be reused.
const SnapshotJobOwnerUIDLabel = "nvidia.com/snapshot-job-uid"

// IsSnapshotJobCompleted reports whether the SnapshotJob's Completed condition is True.
func IsSnapshotJobCompleted(j *SnapshotJob) bool {
	return meta.IsStatusConditionTrue(j.Status.Conditions, SnapshotJobConditionCompleted)
}

// IsSnapshotJobFailed reports whether the SnapshotJob's Failed condition is True.
func IsSnapshotJobFailed(j *SnapshotJob) bool {
	return meta.IsStatusConditionTrue(j.Status.Conditions, SnapshotJobConditionFailed)
}

// IsSnapshotJobTerminal reports whether the SnapshotJob has reached a terminal
// state (Completed=True or Failed=True). Completed objects may still reconcile
// to retry source Job cleanup.
func IsSnapshotJobTerminal(j *SnapshotJob) bool {
	return IsSnapshotJobCompleted(j) || IsSnapshotJobFailed(j)
}

// +kubebuilder:validation:XValidation:rule="self.podSnapshotTemplate.targetContainers.all(c, c in self.podTemplate.spec.containers.map(x, x.name))",message="targetContainers must name containers present in podTemplate"

// SnapshotJobSpec defines the desired state of SnapshotJob.
type SnapshotJobSpec struct {
	// PodTemplate defines the workload to run and capture. The controller injects
	// the snapshot contract (control volume, readiness probe, seccomp, sidecar
	// opt-outs) before creating the batch/v1 Job. Everything else — image,
	// command, GPU resources, sidecars, DRA claims, shared memory — is the
	// caller's responsibility.
	// +kubebuilder:validation:Required
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate"`

	// ActiveDeadlineSeconds bounds the total time allowed for pod scheduling,
	// quiesce, and dump. Applied verbatim to the batch/v1 Job.
	// +optional
	// +kubebuilder:default=3600
	// +kubebuilder:validation:Minimum=1
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// PodSnapshotTemplate defines the properties of the PodSnapshot produced by
	// this job. The controller fills in spec.source from the pod it creates.
	// +kubebuilder:validation:Required
	PodSnapshotTemplate PodSnapshotTemplate `json:"podSnapshotTemplate"`
}

// PodSnapshotTemplate mirrors the PodSnapshot spec fields the user controls. The
// controller fills in spec.source (the pod reference) automatically.
type PodSnapshotTemplate struct {
	// TargetContainers names the container(s) to checkpoint with CRIU. The pod
	// may contain any number of additional containers (helpers, sidecars, etc.)
	// — this field controls only the CRIU dump target.
	//
	// v1alpha1: exactly one target is required. The plural form and nested
	// placement here is intentional: future versions will support per-container
	// config (e.g. per-container quiesceProbe) by extending this list to objects.
	//
	// Defaults to ["main"]. Each entry must be a valid DNS label
	// (1-63 chars, ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$).
	// +optional
	// +kubebuilder:default={"main"}
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	TargetContainers []string `json:"targetContainers,omitempty"`
}

// SnapshotJobStatus defines the observed state of SnapshotJob.
type SnapshotJobStatus struct {
	// PodSnapshotName is the name of the PodSnapshot produced by this job. Set
	// when the PodSnapshot is created. Distinguishes "never created" (empty)
	// from "created but missing" (set, not found).
	// +optional
	PodSnapshotName string `json:"podSnapshotName,omitempty"`

	// StartedAt is when the controller first observed the source pod Ready
	// (job.status.ready > 0), not the pod's own Ready transition time — it can
	// lag the pod's actual transition by up to one reconcile interval.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when a terminal condition (Completed or Failed) was set.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Conditions reflect the current state of the SnapshotJob. Types: Running,
	// Captured, Completed, Failed.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=snapjob
// +kubebuilder:printcolumn:name="Running",type="string",JSONPath=".status.conditions[?(@.type=='Running')].status"
// +kubebuilder:printcolumn:name="Captured",type="string",JSONPath=".status.conditions[?(@.type=='Captured')].status"
// +kubebuilder:printcolumn:name="Completed",type="string",JSONPath=".status.conditions[?(@.type=='Completed')].status"
// +kubebuilder:printcolumn:name="PodSnapshot",type="string",JSONPath=".status.podSnapshotName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SnapshotJob is the Schema for the snapshotjobs API. It fuses running a
// checkpoint-ready workload pod and capturing it into a PodSnapshot into one
// declarative, capture-only object. SnapshotJob has zero awareness of any
// specific consumer (Dynamo, GMS, or otherwise).
//
// No conversion: this type exists only in v1alpha1 (no other API version), so
// it is not part of any conversion scheme.
type SnapshotJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec   SnapshotJobSpec   `json:"spec,omitempty"`
	Status SnapshotJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SnapshotJobList contains a list of SnapshotJob.
type SnapshotJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SnapshotJob `json:"items"`
}
