// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	contentvalidation "k8s.io/apimachinery/pkg/api/validate/content"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/protocol"
)

// buildSourceJob constructs the desired batch/v1 Job for a SnapshotJob's source pod.
// It reuses protocol.NewSourceJob unchanged — that function's body is the agent
// contract (control volume, readiness probe, labels, seccomp, sidecar opt-outs), not
// Dynamo-specific code — and adds only the owner label so the PodSnapshot created
// later (PR 4) can be mapped back to this SnapshotJob without an ownerReference.
//
// No storage is injected (spec §5.3: the agent falls back to its own config).
// The generic source protocol does not wrap the command; the interposer
// PodContract operation below supplies cuda-checkpoint from the configured
// agent image and wraps the opted-in target at its stable absolute path.
func buildSourceJob(sj *snapshotv1alpha1.SnapshotJob) (*batchv1.Job, error) {
	return buildSourceJobWithAgentImage(sj, "")
}

func buildSourceJobWithAgentImage(
	sj *snapshotv1alpha1.SnapshotJob,
	agentImage string,
) (*batchv1.Job, error) {
	// sj.Name is also used as a SnapshotJobOwnerLabel value. Admission caps
	// metadata.name at the label-value limit;
	// retain this check for objects that predate or bypass that schema.
	// IsLabelValue reports the reasons sj.Name fails Kubernetes label-value
	// syntax (RFC 1123: <=63 chars, alphanumeric/'-'/'_'/'.', start/end
	// alphanumeric); empty means valid.
	if errs := contentvalidation.IsLabelValue(sj.Name); len(errs) > 0 {
		return nil, fmt.Errorf("metadata.name %q is not a valid label value: %s", sj.Name, strings.Join(errs, "; "))
	}
	if err := validatePodSnapshotTemplateMetadata(sj); err != nil {
		return nil, err
	}

	targetContainer, err := snapshotJobTargetContainer(sj)
	if err != nil {
		return nil, err
	}

	podTemplate := sj.Spec.PodTemplate.DeepCopy()
	if podTemplate.Labels == nil {
		podTemplate.Labels = map[string]string{}
	}
	podTemplate.Labels[snapshotv1alpha1.SnapshotJobOwnerLabel] = sj.Name
	podTemplate.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] = string(sj.UID)

	job, err := protocol.NewSourceJob(podTemplate, protocol.SourceJobOptions{
		Namespace:             sj.Namespace,
		Name:                  sj.Name,
		TargetContainer:       targetContainer,
		SeccompProfile:        podcontract.DefaultSeccompLocalhostProfile,
		ActiveDeadlineSeconds: sj.Spec.ActiveDeadlineSeconds,
		TTLSecondsAfterFinish: nil,
		WrapLaunchJob:         false,
	})
	if err != nil {
		return nil, err
	}
	if err := podcontract.ShapeCUDAInterposerCapture(
		&job.Spec.Template,
		sj.Spec.PodSnapshotTemplate.TargetContainers,
		agentImage,
	); err != nil {
		return nil, err
	}
	return job, nil
}
