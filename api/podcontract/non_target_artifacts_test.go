// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNonTargetArtifactSubdir(t *testing.T) {
	dir, err := NonTargetArtifactSubdir("0a8c9a75-38a6-4e4e-8f87-e6d637552c25")
	require.NoError(t, err)
	assert.Equal(t, "non-target-artifacts/0a8c9a75-38a6-4e4e-8f87-e6d637552c25", dir)
	for _, uid := range []string{"", ".", "..", "../other", "/other", "job/other", `job\123`} {
		_, err := NonTargetArtifactSubdir(uid)
		require.Error(t, err, uid)
	}
}

func TestNonTargetArtifactContainers(t *testing.T) {
	names, err := NonTargetArtifactContainers(nil)
	require.NoError(t, err)
	assert.Empty(t, names)
	names, err = NonTargetArtifactContainers(map[string]string{"nvidia.com/snapshot-helper-artifact-containers": "saver"})
	require.NoError(t, err)
	assert.Empty(t, names, "the previous annotation must not opt in to the renamed cleanup contract")
	names, err = NonTargetArtifactContainers(map[string]string{NonTargetArtifactContainersAnnotation: " saver, exporter "})
	require.NoError(t, err)
	assert.Equal(t, []string{"saver", "exporter"}, names)
	for _, value := range []string{"", "saver,", "saver,saver", "Bad Name"} {
		_, err := NonTargetArtifactContainers(map[string]string{NonTargetArtifactContainersAnnotation: value})
		require.Error(t, err, value)
	}
}
