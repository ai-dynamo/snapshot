// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
// No storage is injected (spec §5.3: the agent falls back to its own config), and
// WrapLaunchJob is always false (spec §5.4: PodSnapshotTemplate has no field to
// source it from — a caller needing cuda-checkpoint --launch-job wrapping sets it
// up themselves in spec.podTemplate).
func buildSourceJob(sj *snapshotv1alpha1.SnapshotJob) (*batchv1.Job, error) {
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
	if err := injectNonTargetArtifactContract(podTemplate, sj.Spec.NonTargetArtifacts, targetContainer, string(sj.UID)); err != nil {
		return nil, err
	}

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

func injectNonTargetArtifactContract(template *corev1.PodTemplateSpec, policy *snapshotv1alpha1.NonTargetArtifactPolicy, target, uid string) error {
	_, legacyAnnotation := template.Annotations[podcontract.NonTargetArtifactContainersAnnotation]
	if policy != nil && legacyAnnotation {
		return fmt.Errorf("spec.nonTargetArtifacts and the legacy container annotation cannot both be set")
	}
	if policy == nil && !legacyAnnotation {
		return nil
	}
	if policy != nil {
		if len(policy.Containers) == 0 {
			return fmt.Errorf("spec.nonTargetArtifacts.containers must not be empty")
		}
		if template.Annotations == nil {
			template.Annotations = make(map[string]string)
		}
		template.Annotations[podcontract.NonTargetArtifactContainersAnnotation] = strings.Join(policy.Containers, ",")
	}
	nonTargets, err := podcontract.NonTargetArtifactContainers(template.Annotations)
	if err != nil {
		return err
	}
	if policy != nil && len(nonTargets) != len(policy.Containers) {
		return fmt.Errorf("spec.nonTargetArtifacts.containers contains duplicates")
	}
	subdir, err := podcontract.NonTargetArtifactSubdir(uid)
	if err != nil {
		return err
	}
	for _, name := range nonTargets {
		if name == target {
			return fmt.Errorf("spec.nonTargetArtifacts.containers must not name capture target %q", name)
		}
		var nonTarget *corev1.Container
		for i := range template.Spec.Containers {
			if template.Spec.Containers[i].Name == name {
				nonTarget = &template.Spec.Containers[i]
				break
			}
		}
		if nonTarget == nil {
			return fmt.Errorf("spec.nonTargetArtifacts.containers names missing regular non-target container %q", name)
		}
		for _, env := range nonTarget.Env {
			if env.Name == podcontract.SnapshotJobUIDEnv || env.Name == podcontract.NonTargetArtifactSubdirEnv {
				return fmt.Errorf("non-target container %q sets controller-owned environment variable %s", name, env.Name)
			}
		}
		// Kubernetes expands env references in declaration order, so caller
		// variables may refer to these controller-owned values.
		nonTarget.Env = append([]corev1.EnvVar{
			{Name: podcontract.SnapshotJobUIDEnv, Value: uid},
			{Name: podcontract.NonTargetArtifactSubdirEnv, Value: subdir},
		}, nonTarget.Env...)
	}
	return nil
}
