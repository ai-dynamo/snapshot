// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	artifactsDirectory  = "artifacts"
	containersDirectory = "containers"
)

// ResolveArtifactPath returns the checkpoint artifact path owned by one
// PodSnapshotContent and captured container. All variable components must be
// single clean path elements.
func ResolveArtifactPath(basePath, contentUID, containerName string) (string, error) {
	basePath = strings.TrimSpace(basePath)
	if !filepath.IsAbs(basePath) || filepath.Clean(basePath) != basePath {
		return "", fmt.Errorf("base path must be an absolute, clean path: %q", basePath)
	}
	contentUID = strings.TrimSpace(contentUID)
	if err := validatePathElement("PodSnapshotContent UID", contentUID); err != nil {
		return "", err
	}
	containerName = strings.TrimSpace(containerName)
	if err := validatePathElement("container name", containerName); err != nil {
		return "", err
	}
	return filepath.Join(basePath, artifactsDirectory, contentUID, containersDirectory, containerName), nil
}

// ResolveArtifactStagingRoot returns the private staging root for one
// PodSnapshotContent. Keeping it under the same content directory guarantees
// the final rename stays on the same filesystem as the artifact.
func ResolveArtifactStagingRoot(basePath, contentUID string) (string, error) {
	basePath = strings.TrimSpace(basePath)
	if !filepath.IsAbs(basePath) || filepath.Clean(basePath) != basePath {
		return "", fmt.Errorf("base path must be an absolute, clean path: %q", basePath)
	}
	contentUID = strings.TrimSpace(contentUID)
	if err := validatePathElement("PodSnapshotContent UID", contentUID); err != nil {
		return "", err
	}
	return filepath.Join(basePath, artifactsDirectory, contentUID, ".tmp"), nil
}

func validatePathElement(label, value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%s must be a single clean path element: %q", label, value)
	}
	return nil
}
