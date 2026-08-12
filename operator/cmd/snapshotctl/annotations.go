// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// reconcileTargetContainers returns the normalized target-container list.
// A flag value and manifest annotation may both be present only when they match.
// At least one container is always required; maxCount caps the list (0 means uncapped).
func reconcileTargetContainers(annotations map[string]string, flagValue string, maxCount int) ([]string, error) {
	flagNames, flagErr := snapshotv1alpha1.ParseTargetContainers(flagValue)
	if flagErr != nil {
		return nil, fmt.Errorf("--container(s) flag: %w", flagErr)
	}

	manifestRaw := ""
	if annotations != nil {
		manifestRaw = annotations[snapshotv1alpha1.TargetContainersAnnotation]
	}
	manifestNames, manifestErr := snapshotv1alpha1.ParseTargetContainers(manifestRaw)
	if manifestErr != nil {
		return nil, fmt.Errorf("manifest %s annotation: %w", snapshotv1alpha1.TargetContainersAnnotation, manifestErr)
	}

	chosen := flagNames
	if len(flagNames) == 0 {
		chosen = manifestNames
	} else if len(manifestNames) > 0 {
		if snapshotv1alpha1.FormatTargetContainers(flagNames) != snapshotv1alpha1.FormatTargetContainers(manifestNames) {
			return nil, fmt.Errorf(
				"--container(s) flag %q does not match manifest %s %q; pass one or the other",
				snapshotv1alpha1.FormatTargetContainers(flagNames),
				snapshotv1alpha1.TargetContainersAnnotation,
				snapshotv1alpha1.FormatTargetContainers(manifestNames),
			)
		}
	}

	if len(chosen) == 0 {
		return nil, fmt.Errorf("target containers are required: pass --container(s) or set %s on the manifest", snapshotv1alpha1.TargetContainersAnnotation)
	}
	if maxCount > 0 && len(chosen) > maxCount {
		return nil, fmt.Errorf("expected at most %d target container(s), got %d", maxCount, len(chosen))
	}
	return chosen, nil
}
