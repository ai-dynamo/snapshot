// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"path/filepath"
	"strings"
)

const ArtifactsDirectoryName = "artifacts"

// ResolveArtifactsRoot returns the reserved directory containing all
// PodSnapshotContent-owned artifact roots.
func ResolveArtifactsRoot(basePath string) (string, error) {
	if err := ValidateArtifactBasePath(basePath); err != nil {
		return "", err
	}
	return filepath.Join(basePath, ArtifactsDirectoryName), nil
}

// ResolveArtifactRoot returns the complete filesystem root owned by one
// immutable PodSnapshotContent UID.
func ResolveArtifactRoot(basePath, contentUID string) (string, error) {
	artifactsRoot, err := ResolveArtifactsRoot(basePath)
	if err != nil {
		return "", err
	}
	if err := ValidateArtifactPathElement("PodSnapshotContent UID", contentUID); err != nil {
		return "", err
	}
	root := filepath.Join(artifactsRoot, contentUID)
	if err := ValidateArtifactContainment(artifactsRoot, root); err != nil {
		return "", err
	}
	return root, nil
}

// ValidateArtifactBasePath accepts only a clean absolute path other than the
// filesystem root. Symlink checks require filesystem access and are performed
// by the process that mounts and mutates the store.
func ValidateArtifactBasePath(basePath string) error {
	if basePath == "" || !filepath.IsAbs(basePath) || filepath.Clean(basePath) != basePath {
		return fmt.Errorf("artifact base path must be clean and absolute: %q", basePath)
	}
	if basePath == string(filepath.Separator) {
		return fmt.Errorf("artifact base path must not be the filesystem root")
	}
	for _, element := range strings.Split(strings.TrimPrefix(basePath, string(filepath.Separator)), string(filepath.Separator)) {
		if err := ValidateArtifactPathElement("artifact base path component", element); err != nil {
			return err
		}
	}
	return nil
}

// ValidateArtifactPathElement rejects traversal and path separators while
// accepting the characters used by Kubernetes UIDs and container names.
func ValidateArtifactPathElement(label, value string) error {
	if value == "" || value == "." || value == ".." {
		return fmt.Errorf("%s must be a non-traversing path element: %q", label, value)
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("_-.", char)) {
			return fmt.Errorf("%s contains unsupported character %q: %q", label, char, value)
		}
	}
	return nil
}

// ValidateArtifactContainment proves that target is root itself or a child of
// root without relying on string-prefix comparisons.
func ValidateArtifactContainment(root, target string) error {
	if err := ValidateArtifactBasePath(root); err != nil {
		return fmt.Errorf("invalid containment root: %w", err)
	}
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return fmt.Errorf("artifact target must be clean and absolute: %q", target)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("resolve artifact target relative to root: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("artifact target %q escapes root %q", target, root)
	}
	return nil
}
