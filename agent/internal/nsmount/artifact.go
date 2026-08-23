// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

const (
	artifactsDirectory  = "artifacts"
	containersDirectory = "containers"
)

// ResolveArtifactPath returns the checkpoint artifact path owned by one
// PodSnapshotContent and captured container. All variable components must be
// single safe path elements.
func ResolveArtifactPath(basePath, contentUID, containerName string) (string, error) {
	if err := validateAbsolutePath(basePath); err != nil {
		return "", err
	}
	if err := validatePathElement("PodSnapshotContent UID", contentUID); err != nil {
		return "", err
	}
	if err := validatePathElement("container name", containerName); err != nil {
		return "", err
	}
	return filepath.Join(basePath, artifactsDirectory, contentUID, containersDirectory, containerName), nil
}

// ResolveArtifactStagingRoot returns the private staging root for one
// PodSnapshotContent. Checkpoint writes a complete artifact beneath this root
// before renaming it into the final container path. Keeping staging inside the
// content directory guarantees that rename stays on one filesystem, so the
// artifact is published atomically and restore never observes a partial dump.
func ResolveArtifactStagingRoot(basePath, contentUID string) (string, error) {
	if err := validateAbsolutePath(basePath); err != nil {
		return "", err
	}
	if err := validatePathElement("PodSnapshotContent UID", contentUID); err != nil {
		return "", err
	}
	return filepath.Join(basePath, artifactsDirectory, contentUID, ".tmp"), nil
}

func validateAbsolutePath(value string) error {
	if value == "" || value == "/" || value[0] != '/' || path.Clean(value) != value {
		return fmt.Errorf("invalid absolute path %q", value)
	}
	for _, element := range strings.Split(value[1:], "/") {
		if err := validatePathElement("path component", element); err != nil {
			return err
		}
	}
	return nil
}

func validatePathElement(label, value string) error {
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

func validateWithin(root, source string) error {
	if err := validateAbsolutePath(root); err != nil {
		return err
	}
	if err := validateAbsolutePath(source); err != nil {
		return err
	}
	if source != root && !strings.HasPrefix(source, root+"/") {
		return fmt.Errorf("mount source %q must be within %q", source, root)
	}
	return nil
}
