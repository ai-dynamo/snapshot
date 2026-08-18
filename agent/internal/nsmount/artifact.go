// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"path/filepath"

	"github.com/ai-dynamo/snapshot/agent/internal/safepath"
	snapshotprotocol "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// ResolveArtifactPath returns the existing checkpoint artifact layout rooted
// at the agent-owned base path. All variable components must be single clean
// path elements.
func ResolveArtifactPath(basePath, artifactID, version string) (string, error) {
	if err := safepath.ValidateAbsolute("base path", basePath); err != nil {
		return "", err
	}
	if err := safepath.ValidateElement("artifact ID", artifactID); err != nil {
		return "", err
	}
	if version == "" {
		version = snapshotprotocol.DefaultCheckpointArtifactVersion
	}
	if err := safepath.ValidateElement("artifact version", version); err != nil {
		return "", err
	}
	return filepath.Join(basePath, artifactID, "versions", version), nil
}
