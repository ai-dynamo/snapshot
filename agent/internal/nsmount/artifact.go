// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"fmt"
	"path/filepath"
	"strings"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	containersDirectory = "containers"
)

// ResolveArtifactPath returns the checkpoint artifact path owned by one
// PodSnapshotContent and captured container. All variable components must be
// single clean path elements.
func ResolveArtifactPath(basePath, contentUID, containerName string) (string, error) {
	root, err := snapshotv1alpha1.ResolveArtifactRoot(basePath, contentUID)
	if err != nil {
		return "", err
	}
	containerName = strings.TrimSpace(containerName)
	if err := validatePathElement("container name", containerName); err != nil {
		return "", err
	}
	return filepath.Join(root, containersDirectory, containerName), nil
}

// ResolveArtifactStagingRoot returns the private staging root for one
// PodSnapshotContent. Keeping it under the same content directory guarantees
// the final rename stays on the same filesystem as the artifact.
func ResolveArtifactStagingRoot(basePath, contentUID string) (string, error) {
	root, err := snapshotv1alpha1.ResolveArtifactRoot(basePath, contentUID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ".tmp"), nil
}

func validatePathElement(label, value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%s must be a single clean path element: %q", label, value)
	}
	return nil
}
