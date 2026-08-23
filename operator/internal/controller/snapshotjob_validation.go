// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

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
