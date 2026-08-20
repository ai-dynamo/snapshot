// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"fmt"
	"os"
	"path/filepath"
)

// ResolveArtifact composes the artifact path from agent configuration and
// checks it is usable, returning the path the mount will be given.
//
// The path is composed rather than supplied: the storage-location annotation
// this replaced let a pod name any agent-visible directory, so the components
// are validated and the result must contain no symlink. Reading the manifest is
// left to the caller, which uses the ordinary types.ReadManifest.
func ResolveArtifact(basePath, artifactID, version string) (string, error) {
	artifactPath, err := ResolveArtifactPath(basePath, artifactID, version)
	if err != nil {
		return "", err
	}

	// EvalSymlinks both rejects a symlinked component and hands back the
	// canonical name, which is how mount points are recorded and therefore what
	// the nested-mount check has to compare against.
	resolved, err := filepath.EvalSymlinks(artifactPath)
	if err != nil {
		return "", fmt.Errorf("resolve artifact %s: %w", artifactPath, err)
	}
	if resolved != artifactPath {
		return "", fmt.Errorf("artifact %s resolves through a symlink to %s", artifactPath, resolved)
	}
	info, err := os.Stat(artifactPath)
	if err != nil {
		return "", fmt.Errorf("stat artifact %s: %w", artifactPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("artifact %s is not a directory", artifactPath)
	}
	return artifactPath, nil
}
