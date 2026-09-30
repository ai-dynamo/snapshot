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
	for _, tc := range []struct{ name, yaml, wantError string }{
		{name: "native"},
		{name: "identity", yaml: "cuinterpose:\n  frontendSHA256: " + strings.Repeat("a", 64) + "\n  coreSHA256: " + strings.Repeat("b", 64)},
		{name: "obsolete boolean", yaml: "cuinterpose: true", wantError: "obsolete cuinterpose manifest: recreate"},
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
