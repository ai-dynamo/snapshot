// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// snapshotJobTargetContainer returns the single target supported by v1alpha1.
// Admission enforces this cardinality; callers retain the check for objects
// created while admission is unavailable or that predate the current schema.
func snapshotJobTargetContainer(sj *snapshotv1alpha1.SnapshotJob) (string, error) {
	targets := sj.Spec.PodSnapshotTemplate.TargetContainers
	if len(targets) != 1 {
		return "", fmt.Errorf("spec.podSnapshotTemplate.targetContainers must have exactly one entry, got %d", len(targets))
	}
	return targets[0], nil
}

// validatePodSnapshotTemplateMetadata rejects invalid Kubernetes metadata and
// labels reserved for the SnapshotJob controller before the source workload is
// started. The generated PodSnapshot remains the final API-server validation
// boundary; this check turns malformed immutable input into InvalidSpec early.
func validatePodSnapshotTemplateMetadata(sj *snapshotv1alpha1.SnapshotJob) error {
	metadata := sj.Spec.PodSnapshotTemplate.Metadata
	if metadata == nil {
		return nil
	}
	labelsPath := field.NewPath("spec", "podSnapshotTemplate", "metadata", "labels")
	errs := metav1validation.ValidateLabels(metadata.Labels, labelsPath)
	annotationErrs := apivalidation.ValidateAnnotations(
		metadata.Annotations,
		field.NewPath("spec", "podSnapshotTemplate", "metadata", "annotations"),
	)
	errs = append(errs, annotationErrs...)
	for _, reserved := range []string{
		snapshotv1alpha1.SnapshotJobOwnerLabel,
		snapshotv1alpha1.SnapshotJobOwnerUIDLabel,
	} {
		if _, found := metadata.Labels[reserved]; found {
			errs = append(errs, field.Forbidden(labelsPath.Key(reserved), "label is controller-owned"))
		}
	}
	return errs.ToAggregate()
}
