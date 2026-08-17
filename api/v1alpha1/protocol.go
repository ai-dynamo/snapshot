// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const snapshotAnnotationPrefix = "nvidia.com/snapshot-"

// SnapshotAnnotations returns the sorted snapshot-owned annotation keys on a
// pod. Platform and workload annotations are intentionally ignored.
func SnapshotAnnotations(annotations map[string]string) []string {
	keys := make([]string, 0)
	for key := range annotations {
		if key == RestoreFromAnnotation || strings.HasPrefix(key, snapshotAnnotationPrefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// ValidateCaptureAnnotations enforces that capture pods carry no
// snapshot-owned annotations. Capture identity and target container are carried
// by PodSnapshot and PodSnapshotContent instead.
func ValidateCaptureAnnotations(annotations map[string]string) error {
	if keys := SnapshotAnnotations(annotations); len(keys) != 0 {
		return fmt.Errorf("capture pod must not carry snapshot annotations: %s", strings.Join(keys, ", "))
	}
	return nil
}

// RestoreFromAnnotations validates the restore annotation contract and returns
// the same-namespace PodSnapshot name. Exactly one snapshot-owned annotation is
// accepted; unrelated workload and platform annotations remain valid.
func RestoreFromAnnotations(annotations map[string]string) (string, error) {
	keys := SnapshotAnnotations(annotations)
	if len(keys) != 1 || keys[0] != RestoreFromAnnotation {
		return "", fmt.Errorf("restore pod must carry exactly one snapshot annotation, %s; found %v", RestoreFromAnnotation, keys)
	}
	snapshotName := strings.TrimSpace(annotations[RestoreFromAnnotation])
	if snapshotName == "" {
		return "", fmt.Errorf("%s must name a PodSnapshot", RestoreFromAnnotation)
	}
	if errs := validation.IsDNS1123Subdomain(snapshotName); len(errs) != 0 {
		return "", fmt.Errorf("%s value %q is not a valid PodSnapshot name: %s", RestoreFromAnnotation, snapshotName, strings.Join(errs, "; "))
	}
	return snapshotName, nil
}
