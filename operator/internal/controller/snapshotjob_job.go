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

// buildSourceJob constructs the desired Job with no tool delivery configured,
// for callers and tests that run without a reconciler. A target that may use
// more than one GPU then fails: the launch-job wrapper needs an agent image.
func buildSourceJob(sj *snapshotv1alpha1.SnapshotJob) (*batchv1.Job, error) {
	job, _, err := buildShapedSourceJob(sj, podcontract.CUDAToolsDelivery{}, nil)
	return job, err
}

// buildShapedSourceJob constructs the desired batch/v1 Job for a SnapshotJob's
// source pod. It reuses protocol.NewSourceJob unchanged — that function's body
// is the agent contract (control volume, readiness probe, labels, seccomp,
// sidecar opt-outs), not Dynamo-specific code — adds the owner label so the
// PodSnapshot created later can be mapped back to this SnapshotJob without an
// ownerReference, and applies the launch-job contract to every target that may
// use more than one GPU: cuda-checkpoint is copied out of the agent image and
// the target is launched under cuda-checkpoint --launch-job, which the agent
// requires to checkpoint a multi-GPU process tree. DRA claims are sized through
// claims; an unresolvable claim counts as multi-GPU. Returns the targets that
// carry the wrapper. A template that opts into cuinterpose additionally gets
// the shim preloaded, and every target wrapped whatever its GPU count.
//
// No storage is injected (spec §5.3: the agent falls back to its own config).
func buildShapedSourceJob(
	sj *snapshotv1alpha1.SnapshotJob,
	delivery podcontract.CUDAToolsDelivery,
	claims podcontract.ClaimDevices,
) (*batchv1.Job, []string, error) {
	// sj.Name is also used as a SnapshotJobOwnerLabel value. Admission caps
	// metadata.name at the label-value limit;
	// retain this check for objects that predate or bypass that schema.
	// IsLabelValue reports the reasons sj.Name fails Kubernetes label-value
	// syntax (RFC 1123: <=63 chars, alphanumeric/'-'/'_'/'.', start/end
	// alphanumeric); empty means valid.
	if errs := contentvalidation.IsLabelValue(sj.Name); len(errs) > 0 {
		return nil, nil, fmt.Errorf("metadata.name %q is not a valid label value: %s", sj.Name, strings.Join(errs, "; "))
	}
	if err := validatePodSnapshotTemplateMetadata(sj); err != nil {
		return nil, nil, err
	}

	targetContainer, err := snapshotJobTargetContainer(sj)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}
	wrapped, err := podcontract.ShapeCUDALaunchJob(
		&job.Spec.Template,
		sj.Spec.PodSnapshotTemplate.TargetContainers,
		delivery,
		claims,
	)
	if err != nil {
		return nil, nil, err
	}
	if err := podcontract.ShapeCuinterposeCapture(
		&job.Spec.Template,
		sj.Spec.PodSnapshotTemplate.TargetContainers,
		delivery,
	); err != nil {
		return nil, nil, err
	}
	return job, wrapped, nil
}
