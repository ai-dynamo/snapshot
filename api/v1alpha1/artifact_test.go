// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveArtifactRoot(t *testing.T) {
	root, err := ResolveArtifactRoot("/checkpoints", "content-uid")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join("/checkpoints", "artifacts", "content-uid"), root)
}

func TestResolveArtifactRootRejectsUnsafeInput(t *testing.T) {
	for name, args := range map[string][2]string{
		"relative base": {"checkpoints", "content-uid"},
		"unclean base":  {"/checkpoints/../escape", "content-uid"},
		"empty uid":     {"/checkpoints", ""},
		"nested uid":    {"/checkpoints", "other/content"},
		"dot-dot uid":   {"/checkpoints", ".."},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveArtifactRoot(args[0], args[1])
			require.Error(t, err)
		})
	}
}

func TestCaptureLeaseName(t *testing.T) {
	assert.Equal(t,
		"snapshot-capture-6d661f8ff51152ea93b44e3e8ea2dc98",
		CaptureLeaseName("content-uid", "main"),
	)
	assert.NotEqual(t,
		CaptureLeaseName("content-uid", "main"),
		CaptureLeaseName("content-uid", "sidecar"),
	)
}
