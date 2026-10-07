// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelperArtifactSubdir(t *testing.T) {
	dir, err := HelperArtifactSubdir("0a8c9a75-38a6-4e4e-8f87-e6d637552c25")
	require.NoError(t, err)
	assert.Equal(t, "helper-artifacts/0a8c9a75-38a6-4e4e-8f87-e6d637552c25", dir)
	for _, uid := range []string{"", ".", "..", "../other", "/other", "job/other", `job\other`} {
		_, err := HelperArtifactSubdir(uid)
		require.Error(t, err, uid)
	}
}

func TestHelperArtifactContainers(t *testing.T) {
	names, err := HelperArtifactContainers(nil)
	require.NoError(t, err)
	assert.Empty(t, names)
	names, err = HelperArtifactContainers(map[string]string{HelperArtifactContainersAnnotation: " saver, exporter "})
	require.NoError(t, err)
	assert.Equal(t, []string{"saver", "exporter"}, names)
	for _, value := range []string{"", "saver,", "saver,saver", "Bad Name"} {
		_, err := HelperArtifactContainers(map[string]string{HelperArtifactContainersAnnotation: value})
		require.Error(t, err, value)
	}
}
