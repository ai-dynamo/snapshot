// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCuInterposeManifest(t *testing.T) {
	identity := "cuinterpose:\n  libraries:\n    libcuinterpose.so:\n      sha256: " + strings.Repeat("a", 64) +
		"\n    libcuinterpose_core.so:\n      sha256: " + strings.Repeat("b", 64)
	for _, tc := range []struct{ name, yaml, wantError string }{
		{name: "native"},
		{name: "identity", yaml: identity + "\n  pids: []"},
		{name: "missing pids", yaml: identity, wantError: "pids must be present"},
		{name: "null pids", yaml: identity + "\n  pids: null", wantError: "pids must be present"},
		{name: "not native", yaml: identity + "\n  pids: [7]", wantError: "not a CUDA participant"},
		{name: "missing hashes", yaml: "cuinterpose: {}", wantError: "must contain exactly"},
		{name: "missing core", yaml: "cuinterpose:\n  libraries:\n    libcuinterpose.so:\n      sha256: " + strings.Repeat("a", 64), wantError: "must contain exactly"},
		{name: "unknown library", yaml: "cuinterpose:\n  libraries:\n    libcuinterpose.so:\n      sha256: " + strings.Repeat("a", 64) +
			"\n    libother.so:\n      sha256: " + strings.Repeat("b", 64), wantError: "libraries[libcuinterpose_core.so].sha256"},
		{name: "malformed hash", yaml: strings.Replace(identity, strings.Repeat("a", 64), "not-a-hash", 1), wantError: "libraries[libcuinterpose.so].sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(directory, manifestFilename),
				[]byte("artifact:\n  contentUID: content\n  containerName: main\n"+tc.yaml+"\n"), 0600))
			manifest, err := ReadManifest(directory)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.yaml != "", manifest.CuInterpose != nil)
			require.NoError(t, WriteManifest(directory, manifest))
			loaded, err := ReadManifest(directory)
			require.NoError(t, err)
			require.Equal(t, manifest.CuInterpose, loaded.CuInterpose)
		})
	}
}

func TestCuInterposeParticipantManifest(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pids      []int
		wantError string
	}{
		{name: "subset", pids: []int{623}},
		{name: "all frontend only", pids: []int{}},
		{name: "missing", wantError: "pids must be present"},
		{name: "zero", pids: []int{0}, wantError: "positive, unique"},
		{name: "negative", pids: []int{-1}, wantError: "positive, unique"},
		{name: "duplicate", pids: []int{623, 623}, wantError: "positive, unique"},
		{name: "not native", pids: []int{622}, wantError: "not a CUDA participant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := &CheckpointManifest{
				Artifact: ArtifactManifest{ContentUID: "content", ContainerName: "main"},
				CUDA:     CUDAManifest{PIDs: []int{1, 623}},
				CuInterpose: &CuInterposeManifest{
					Libraries: map[string]CuInterposeLibraryIdentity{
						CuInterposeFrontend: {SHA256: strings.Repeat("a", 64)},
						CuInterposeCore:     {SHA256: strings.Repeat("b", 64)},
					},
					PIDs: tc.pids,
				},
			}
			directory := t.TempDir()
			err := WriteManifest(directory, manifest)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			loaded, err := ReadManifest(directory)
			require.NoError(t, err)
			require.Equal(t, []int{1, 623}, loaded.CUDA.PIDs)
			require.Equal(t, tc.pids, loaded.CuInterpose.PIDs)
			require.Equal(t, len(tc.pids) > 0, loaded.CuInterpose.UsesCoordinator())
		})
	}
}
