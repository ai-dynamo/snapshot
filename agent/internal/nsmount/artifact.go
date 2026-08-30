// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"fmt"
	"path/filepath"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	containersDirectory = "containers"
)

// ResolveArtifactPath returns the checkpoint artifact path owned by one
// PodSnapshotContent and captured container. All variable components must be
// single safe path elements.
func ResolveArtifactPath(basePath, contentUID, containerName string) (string, error) {
	root, err := snapshotv1alpha1.ResolveArtifactRoot(basePath, contentUID)
	if err != nil {
		return "", err
	}
	if err := snapshotv1alpha1.ValidateArtifactPathElement("container name", containerName); err != nil {
		return "", err
	}
	return filepath.Join(root, containersDirectory, containerName), nil
}

// ResolveArtifactStagingRoot returns the private staging root for one
// PodSnapshotContent. Checkpoint writes a complete artifact beneath this root
// before renaming it into the final container path. Keeping staging inside the
// content directory guarantees that rename stays on one filesystem, so the
// artifact is published atomically and restore never observes a partial dump.
func ResolveArtifactStagingRoot(basePath, contentUID string) (string, error) {
	root, err := snapshotv1alpha1.ResolveArtifactRoot(basePath, contentUID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ".tmp"), nil
}

func validateWithin(root, source string) error {
	if err := snapshotv1alpha1.ValidateArtifactContainment(root, source); err != nil {
		return fmt.Errorf("mount source containment: %w", err)
	}
	return nil
}
