// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
)

const artifactsDirectory = "artifacts"

// ResolveArtifactRoot returns the directory owned by one PodSnapshotContent.
// The UID is validated as one clean path element so callers can safely remove
// the returned tree during finalization.
func ResolveArtifactRoot(basePath, contentUID string) (string, error) {
	basePath = strings.TrimSpace(basePath)
	if !filepath.IsAbs(basePath) || filepath.Clean(basePath) != basePath {
		return "", fmt.Errorf("base path must be an absolute, clean path: %q", basePath)
	}
	contentUID = strings.TrimSpace(contentUID)
	if contentUID == "" || contentUID == "." || contentUID == ".." ||
		filepath.Base(contentUID) != contentUID || strings.ContainsAny(contentUID, `/\`) {
		return "", fmt.Errorf("PodSnapshotContent UID must be a single clean path element: %q", contentUID)
	}
	return filepath.Join(basePath, artifactsDirectory, contentUID), nil
}

// CaptureLeaseName returns the DNS-safe Lease name for one immutable
// content/container artifact identity. The agent and operator share this
// derivation so deletion can wait for an in-flight capture.
func CaptureLeaseName(contentUID, containerName string) string {
	digest := sha256.Sum256([]byte(contentUID + "\x00" + containerName))
	return fmt.Sprintf("snapshot-capture-%x", digest[:16])
}
