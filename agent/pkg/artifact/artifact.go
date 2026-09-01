// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package artifact defines the shared filesystem layout for snapshot artifacts.
package artifact

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const directoryName = "artifacts"

// ResolveRoot returns the reserved directory containing all content roots.
func ResolveRoot(basePath string) (string, error) {
	if err := ValidateBasePath(basePath); err != nil {
		return "", err
	}
	return filepath.Join(basePath, directoryName), nil
}

// ResolveContentRoot returns the complete filesystem root owned by one immutable content UID.
func ResolveContentRoot(basePath, contentUID string) (string, error) {
	artifactsRoot, err := ResolveRoot(basePath)
	if err != nil {
		return "", err
	}
	if err := ValidatePathElement("PodSnapshotContent UID", contentUID); err != nil {
		return "", err
	}
	root := filepath.Join(artifactsRoot, contentUID)
	if err := ValidateContainment(artifactsRoot, root); err != nil {
		return "", err
	}
	return root, nil
}

// ValidateBasePath accepts only a clean absolute path other than the filesystem root.
func ValidateBasePath(basePath string) error {
	if basePath == "" || !filepath.IsAbs(basePath) || filepath.Clean(basePath) != basePath {
		return fmt.Errorf("artifact base path must be clean and absolute: %q", basePath)
	}
	if basePath == string(filepath.Separator) {
		return fmt.Errorf("artifact base path must not be the filesystem root")
	}
	pathWithoutRoot := strings.TrimPrefix(basePath, string(filepath.Separator))
	for _, element := range strings.Split(pathWithoutRoot, string(filepath.Separator)) {
		if err := ValidatePathElement("artifact base path component", element); err != nil {
			return err
		}
	}
	return nil
}

// ValidatePathElement rejects traversal and path separators while accepting Kubernetes UIDs and container names.
func ValidatePathElement(label, value string) error {
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

// ValidateContainment proves that target is root itself or a child of root.
func ValidateContainment(root, target string) error {
	if err := ValidateBasePath(root); err != nil {
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

// ValidateDirectory verifies that path is an ordinary directory, not a symlink.
func ValidateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect artifact directory %q: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifact directory %q must be a non-symlink directory", path)
	}
	return nil
}
