// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	snapshotprotocol "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// ResolveArtifactPath returns the existing checkpoint artifact layout rooted
// at the agent-owned base path. All variable components must be single clean
// path elements.
func ResolveArtifactPath(basePath, artifactID, version string) (string, error) {
	if err := validateAbsolutePath(basePath); err != nil {
		return "", err
	}
	if err := validatePathElement("artifact ID", artifactID); err != nil {
		return "", err
	}
	if version == "" {
		version = snapshotprotocol.DefaultCheckpointArtifactVersion
	}
	if err := validatePathElement("artifact version", version); err != nil {
		return "", err
	}
	return filepath.Join(basePath, artifactID, "versions", version), nil
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
	if source == root || !strings.HasPrefix(source, root+"/") {
		return fmt.Errorf("mount source %q must be within %q", source, root)
	}
	return nil
}
