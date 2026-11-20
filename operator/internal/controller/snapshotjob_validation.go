// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

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
// controller-owned labels before the source workload is started. The
// generated PodSnapshot remains the final API-server validation boundary; this
// check turns malformed immutable input into InvalidSpec early.
func validatePodSnapshotTemplateMetadata(sj *snapshotv1alpha1.SnapshotJob) error {
	return validateTemplateMetadata(sj.Spec.PodSnapshotTemplate.Metadata, field.NewPath("spec", "podSnapshotTemplate", "metadata"))
}
