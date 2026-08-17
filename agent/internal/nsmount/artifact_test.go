// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveArtifactPath(t *testing.T) {
	t.Parallel()

	got, err := ResolveArtifactPath("/checkpoints", "content-uid-123", "main")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/checkpoints", "artifacts", "content-uid-123", "containers", "main"), got)

	staging, err := ResolveArtifactStagingRoot("/checkpoints", "content-uid-123")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/checkpoints", "artifacts", "content-uid-123", ".tmp"), staging)
}

func TestResolveArtifactPathRejectsUnsafeCoordinates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, basePath, contentUID, containerName string
	}{
		{name: "relative base", basePath: "checkpoints", contentUID: "content-uid-123", containerName: "main"},
		{name: "unclean base", basePath: "/checkpoints/../etc", contentUID: "content-uid-123", containerName: "main"},
		{name: "content traversal", basePath: "/checkpoints", contentUID: "..", containerName: "main"},
		{name: "content separator", basePath: "/checkpoints", contentUID: "a/b", containerName: "main"},
		{name: "container traversal", basePath: "/checkpoints", contentUID: "content-uid-123", containerName: ".."},
		{name: "container separator", basePath: "/checkpoints", contentUID: "content-uid-123", containerName: "a/b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveArtifactPath(tc.basePath, tc.contentUID, tc.containerName)
			require.Error(t, err)
		})
	}
}
