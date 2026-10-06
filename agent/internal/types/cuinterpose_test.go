// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestCuInterposeManifest(t *testing.T) {
	for _, tc := range []struct{ name, yaml, wantError string }{
		{name: "native"},
		{name: "identity", yaml: "cuinterpose:\n  frontendSHA256: " + strings.Repeat("a", 64) + "\n  coreSHA256: " + strings.Repeat("b", 64) + "\n  pids: []"},
		{name: "missing pids", yaml: "cuinterpose:\n  frontendSHA256: " + strings.Repeat("a", 64) + "\n  coreSHA256: " + strings.Repeat("b", 64), wantError: "pids must be present"},
		{name: "null pids", yaml: "cuinterpose:\n  frontendSHA256: " + strings.Repeat("a", 64) + "\n  coreSHA256: " + strings.Repeat("b", 64) + "\n  pids: null", wantError: "pids must be present"},
		{name: "invalid type", yaml: "cuinterpose: true", wantError: "cannot unmarshal !!bool"},
		{name: "missing hashes", yaml: "cuinterpose: {}", wantError: "frontendSHA256"},
		{name: "missing core", yaml: "cuinterpose:\n  frontendSHA256: " + strings.Repeat("a", 64), wantError: "coreSHA256"},
		{name: "malformed hash", yaml: "cuinterpose:\n  frontendSHA256: not-a-hash", wantError: "frontendSHA256"},
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
			require.Equal(t, tc.name == "identity", manifest.CuInterpose != nil)
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
					FrontendSHA256: strings.Repeat("a", 64), CoreSHA256: strings.Repeat("b", 64), PIDs: tc.pids,
				},
			}
			directory := t.TempDir()
			err := WriteManifest(directory, manifest)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				// Marshal directly to also exercise rejection at the restore boundary.
				content, err := yaml.Marshal(manifest)
				require.NoError(t, err)
				if tc.pids == nil {
					content = []byte(strings.ReplaceAll(string(content), "pids: []", "pids: null"))
				}
				require.NoError(t, os.WriteFile(filepath.Join(directory, manifestFilename), content, 0600))
				_, err = ReadManifest(directory)
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			loaded, err := ReadManifest(directory)
			require.NoError(t, err)
			require.Equal(t, []int{1, 623}, loaded.CUDA.PIDs)
			require.Equal(t, tc.pids, loaded.CuInterpose.PIDs)
			require.Equal(t, len(tc.pids) > 0, loaded.CuInterpose.HasRuntime())
		})
	}
}
