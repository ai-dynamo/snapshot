/*
 * SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PodSnapshot and PodSnapshotContent status condition types. Both objects share this
// vocabulary; the operator and node agent set them via meta.SetStatusCondition.
const (
	// PodSnapshotConditionReady is True when the checkpoint and binding completed and
	// the snapshot is usable for restore.
	PodSnapshotConditionReady = "Ready"
	// PodSnapshotConditionFailed is True when the checkpoint or binding failed terminally.
	PodSnapshotConditionFailed = "Failed"
)

// IsPodSnapshotSucceeded reports whether the PodSnapshot's Ready condition is True.
func IsPodSnapshotSucceeded(s *PodSnapshot) bool {
	return meta.IsStatusConditionTrue(s.Status.Conditions, PodSnapshotConditionReady)
}

// IsPodSnapshotFailed reports whether the PodSnapshot's Failed condition is True.
func IsPodSnapshotFailed(s *PodSnapshot) bool {
	return meta.IsStatusConditionTrue(s.Status.Conditions, PodSnapshotConditionFailed)
}

// PodSnapshotSpec defines the desired state of PodSnapshot.
// +kubebuilder:validation:XValidation:rule="!has(self.containers) || self.containers.all(c, c.name in (has(self.source.artifact) ? self.source.artifact.containers : self.source.podRef.containers))",message="containers must name containers in source.podRef.containers or source.artifact.containers"
type PodSnapshotSpec struct {
	// Source identifies the checkpointed workload. It is a struct (rather than an
	// inlined reference) so future source variants can be added additively.
	// +kubebuilder:validation:Required
	Source PodSnapshotSource `json:"source"`

	// CheckpointRetryLimit is how many times a failed checkpoint may be retried in
	// place, over and above the first attempt: 0 means one attempt and no retry.
	// Unset means this request expressed no preference and the agent's
	// cluster-wide default applies, which is distinct from an explicit 0.
	// +optional
	// +kubebuilder:validation:Minimum=0
	CheckpointRetryLimit *int32 `json:"checkpointRetryLimit,omitempty"`

	// Containers holds settings for the checkpointed containers, matched by
	// name to source.podRef.containers, or for an import to
	// source.artifact.containers. It is a separate list so settings stay the
	// same when other kinds of source are added.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=1
	Containers []ContainerSnapshotConfig `json:"containers,omitempty"`
}

// PodSnapshotSource identifies where a PodSnapshot's checkpoint comes from: a
// pod to checkpoint, or an artifact that already exists. Exactly one is set.
// +kubebuilder:validation:XValidation:rule="has(self.podRef) != has(self.artifact)",message="exactly one of podRef or artifact is required"
type PodSnapshotSource struct {
	// PodRef references the pod, in the PodSnapshot's namespace, that is checkpointed.
	// The operator prepares the pod (the snapshot-control volume) before creating
	// the PodSnapshot; the snapshot store is mounted by the node agent, not the
	// pod. The target container is carried on PodRef.Containers, not a pod
	// annotation.
	// +optional
	PodRef *PodReference `json:"podRef,omitempty"`

	// Artifact imports a checkpoint taken elsewhere — in another cluster, or in
	// this one before its objects were lost — instead of checkpointing a pod.
	// The operator binds the PodSnapshot to a PodSnapshotContent that names the
	// artifact and marks both Ready without a checkpoint: Ready means the
	// reference is accepted, not that the artifact's bytes are anywhere. They
	// reach a node's snapshot store by other means, and a restore on a node
	// that does not hold them yet waits for them.
	// +optional
	Artifact *ArtifactReference `json:"artifact,omitempty"`
}

// TargetContainers returns the checkpointed container names, whichever source is set.
func (s *PodSnapshotSource) TargetContainers() []string {
	switch {
	case s.Artifact != nil:
		return s.Artifact.Containers
	case s.PodRef != nil:
		return s.PodRef.Containers
	default:
		return nil
	}
}

// ArtifactReference names an existing checkpoint artifact by the identity it
// was taken under. Nothing in it is chosen at import: both fields are read off
// the artifact, artifacts/<uid>/containers/<container>/.
type ArtifactReference struct {
	// UID is the UID of the PodSnapshotContent the artifact was checkpointed
	// for, in whatever cluster that was. It names the artifact's directory in
	// the snapshot store and is recorded in the artifact's manifest, which
	// restore checks it against, so it is the original UID and never the
	// importing content's own.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	UID types.UID `json:"uid"`

	// Containers names the checkpointed container, as for PodReference.Containers.
	// v1alpha1 supports exactly one.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Containers []string `json:"containers"`
}

// PodReference names a pod in the same namespace as the referencing PodSnapshot.
type PodReference struct {
	// Name of the source pod.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID of the source pod, recorded so the node agent dumps that specific
	// pod and not a same-named recreation.
	// +optional
	UID types.UID `json:"uid,omitempty"`

	// Containers narrows the checkpoint to these containers of the source pod. The
	// node agent reads this instead of the source pod's target-container
	// annotation. v1alpha1 supports exactly one container; the cap is lifted when
	// the runtime supports more than one.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Containers []string `json:"containers"`
}

// PodSnapshotStatus defines the observed state of PodSnapshot.
type PodSnapshotStatus struct {
	// BoundPodSnapshotContentName is the name of the cluster-scoped PodSnapshotContent
	// this PodSnapshot is bound to. It is nil until the agent has created the
	// content and recorded the binding.
	// +optional
	BoundPodSnapshotContentName *string `json:"boundSnapshotContentName,omitempty"`

	// Conditions reflect the latest observations of the PodSnapshot's state.
	// Standard types are Ready and Failed.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=podsnap
// +kubebuilder:printcolumn:name="Content",type="string",JSONPath=".status.boundSnapshotContentName",description="Bound PodSnapshotContent"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Ready condition"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PodSnapshot is the Schema for the snapshots API. It is the namespaced binding
// for one container's snapshot and is consumed by restore paths.
//
// No conversion: this type exists only in v1alpha1 (no other API version), so it
// is not part of any conversion scheme.
type PodSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
	Spec   PodSnapshotSpec   `json:"spec,omitempty"`
	Status PodSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PodSnapshotList contains a list of PodSnapshot.
type PodSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PodSnapshot `json:"items"`
}
