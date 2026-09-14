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

// buildBaseSourceJob constructs the source Job before Snapshot's configured
// CUDA tools are added.
func buildBaseSourceJob(sj *snapshotv1alpha1.SnapshotJob) (*batchv1.Job, error) {
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

	return protocol.NewSourceJob(podTemplate, protocol.SourceJobOptions{
		Namespace:             sj.Namespace,
		Name:                  sj.Name,
		TargetContainer:       targetContainer,
		SeccompProfile:        podcontract.DefaultSeccompLocalhostProfile,
		ActiveDeadlineSeconds: sj.Spec.ActiveDeadlineSeconds,
		TTLSecondsAfterFinish: nil,
		WrapLaunchJob:         false,
	})
}

// buildShapedSourceJob adds Snapshot's CUDA tools to every target of the base
// source Job and wraps only targets that may use more than one GPU. DRA claims
// are sized through claims; an unresolvable claim counts as multi-GPU. No
// storage is injected (spec §5.3: the agent falls back to its own config).
func buildShapedSourceJob(
	sj *snapshotv1alpha1.SnapshotJob,
	delivery podcontract.CUDAToolsDelivery,
	claims podcontract.ClaimDevices,
) (*batchv1.Job, []string, error) {
	job, err := buildBaseSourceJob(sj)
	if err != nil {
		return nil, nil, err
	}
	wrapped, err := podcontract.ShapeCUDATools(
		&job.Spec.Template,
		sj.Spec.PodSnapshotTemplate.TargetContainers,
		delivery,
		claims,
	)
	if err != nil {
		return nil, nil, err
	}
	return job, wrapped, nil
}
