// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// GetRestoreFromSnapshotName returns the same-namespace PodSnapshot named by
// the restore-from annotation. All other annotations are inert metadata.
func GetRestoreFromSnapshotName(annotations map[string]string) (string, error) {
	snapshotName := strings.TrimSpace(annotations[RestoreFromAnnotation])
	if snapshotName == "" {
		return "", fmt.Errorf("%s must name a PodSnapshot", RestoreFromAnnotation)
	}
	if errs := validation.IsDNS1123Subdomain(snapshotName); len(errs) != 0 {
		return "", fmt.Errorf("%s value %q is not a valid PodSnapshot name: %s", RestoreFromAnnotation, snapshotName, strings.Join(errs, "; "))
	}
	return snapshotName, nil
}

// SkipCompatCheckFromAnnotations reports whether this pod asks for the restore
// compatibility gate to be skipped. Anything that is not a recognized true
// keeps the gate on, so a typo cannot silently disable it.
func SkipCompatCheckFromAnnotations(annotations map[string]string) bool {
	skip, err := strconv.ParseBool(strings.TrimSpace(annotations[SkipCompatCheckAnnotation]))
	return err == nil && skip
}
