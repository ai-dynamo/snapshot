// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	contentvalidation "k8s.io/apimachinery/pkg/api/validate/content"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/protocol"
)

// sourceJobArtifactVersion satisfies protocol.CheckpointJobOptions.ArtifactVersion,
// which the agent uses as the versions/<N> path segment under the artifact's
// base path (see ResolveCheckpointStorage). SnapshotJob has no concept of
// artifact versioning; the constant exists only because the shared option
// struct still carries the field. If ArtifactVersion is ever repurposed for
// real multi-version artifacts, this call site needs a real value here, not
// a hardcoded "1".
const sourceJobArtifactVersion = "1"

// buildSourceJob constructs the desired batch/v1 Job for a SnapshotJob's source pod.
// It reuses protocol.NewCheckpointJob unchanged — that function's body is the agent
// contract (control volume, readiness probe, labels, seccomp, sidecar opt-outs), not
// Dynamo-specific code — and adds only the owner label so the PodSnapshot created
// later (PR 4) can be mapped back to this SnapshotJob without an ownerReference.
//
// No storage is injected (spec §5.3: the agent falls back to its own config), and
// WrapLaunchJob is always false (spec §5.4: PodSnapshotTemplate has no field to
// source it from — a caller needing cuda-checkpoint --launch-job wrapping sets it
// up themselves in spec.podTemplate).
func buildSourceJob(sj *snapshotv1alpha1.SnapshotJob) (*batchv1.Job, error) {
	// sj.Name is also used as a label value (SnapshotJobOwnerLabel,
	// CheckpointIDLabel); the CRD doesn't cap metadata.name length, so this must
	// be checked here or a long name fails Job creation forever.
	// IsLabelValue reports the reasons sj.Name fails Kubernetes label-value
	// syntax (RFC 1123: <=63 chars, alphanumeric/'-'/'_'/'.', start/end
	// alphanumeric); empty means valid.
	if errs := contentvalidation.IsLabelValue(sj.Name); len(errs) > 0 {
		return nil, fmt.Errorf("metadata.name %q is not a valid label value: %s", sj.Name, strings.Join(errs, "; "))
	}

	targetContainers := sj.Spec.PodSnapshotTemplate.TargetContainers
	if len(targetContainers) != 1 {
		return nil, fmt.Errorf("spec.podSnapshotTemplate.targetContainers must have exactly one entry, got %d", len(targetContainers))
	}

	podTemplate := sj.Spec.PodTemplate.DeepCopy()
	if podTemplate.Labels == nil {
		podTemplate.Labels = map[string]string{}
	}
	podTemplate.Labels[snapshotv1alpha1.SnapshotJobOwnerLabel] = sj.Name
	podTemplate.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] = string(sj.UID)

	return protocol.NewCheckpointJob(podTemplate, protocol.CheckpointJobOptions{
		Namespace:             sj.Namespace,
		Name:                  sj.Name,
		TargetContainer:       targetContainers[0],
		CheckpointID:          sj.Name,
		ArtifactVersion:       sourceJobArtifactVersion,
		SeccompProfile:        snapshotv1alpha1.DefaultSeccompLocalhostProfile,
		ActiveDeadlineSeconds: sj.Spec.ActiveDeadlineSeconds,
		TTLSecondsAfterFinish: nil,
		WrapLaunchJob:         false,
	})
}
