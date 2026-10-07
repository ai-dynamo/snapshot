// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const pvcStorageYAML = `pvc:
  namespace: snapshot
  claimName: snapshot-pvc
  basePath: /
`

func TestNewContentBindingIsAnExplicitRolloutStep(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "storage.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(pvcStorageYAML), 0o600))
	storeID, err := configuredStoreID(configPath)
	require.NoError(t, err)
	require.NotEmpty(t, storeID)

	// An operator-first upgrade can read store identity for cleanup without issuing bound captures to old agents.
	captureStoreID, err := newContentStoreID(storeID, false)
	require.NoError(t, err)
	assert.Empty(t, captureStoreID)

	// Activate only after the compatible agent/PageBroker rollout has completed.
	captureStoreID, err = newContentStoreID(storeID, true)
	require.NoError(t, err)
	assert.Equal(t, storeID, captureStoreID)

	_, err = newContentStoreID("", true)
	require.ErrorContains(t, err, "requires snapshot-storage-config")
}

func TestConfiguredStoreIDPreservesLegacyStartup(t *testing.T) {
	id, err := configuredStoreID("")
	require.NoError(t, err)
	assert.Empty(t, id)
}

func TestMustStoreIDReturnsID(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "storage.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(pvcStorageYAML), 0o600))
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "legacy"},
		{
			name: "configured",
			path: configPath,
			want: "store-v1-57e06c9609c92e973d048143531baadb9271a9571f1d1dbc21af2c6cd13d24c2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mustStoreID(tc.path))
		})
	}
}

func TestMustStoreIDExitsOnInvalidConfiguration(t *testing.T) {
	// Exercise os.Exit in a subprocess so it cannot terminate the test runner.
	if os.Getenv("SNAPSHOT_TEST_STORAGE_CONFIG_EXIT") == "1" {
		ctrl.SetLogger(zap.New())
		mustStoreID(os.Getenv("SNAPSHOT_TEST_STORAGE_CONFIG_PATH"))
		os.Exit(0) // Returning instead of exiting must fail the parent assertion.
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), executable,
		"-test.run=^TestMustStoreIDExitsOnInvalidConfiguration$")
	cmd.Env = append(os.Environ(), "SNAPSHOT_TEST_STORAGE_CONFIG_EXIT=1",
		"SNAPSHOT_TEST_STORAGE_CONFIG_PATH="+filepath.Join(t.TempDir(), "missing.yaml"))
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Contains(t, string(output), "invalid storage configuration")
}

func TestConfiguredStoreID(t *testing.T) {
	const expected = "store-v1-57e06c9609c92e973d048143531baadb9271a9571f1d1dbc21af2c6cd13d24c2"
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"PVC identity", pvcStorageYAML, ""},
		{"normalized root", strings.Replace(pvcStorageYAML, "basePath: /", "basePath: ///./", 1), ""},
		{"missing backend", "{}", "PVC storage identity is required"},
		{"S3 not enabled", "s3:\n  bucket: b\n", "decode storage configuration"},
		{"type discriminator", "type: pvc\n" + pvcStorageYAML, "decode storage configuration"},
		{"null identity", "pvc: null", "PVC storage identity is required"},
		{"empty identity", "pvc: {}", "PVC namespace"},
		{"missing namespace", strings.Replace(pvcStorageYAML, "  namespace: snapshot\n", "", 1), "PVC namespace"},
		{"missing claim", strings.Replace(pvcStorageYAML, "  claimName: snapshot-pvc\n", "", 1), "PVC claim name"},
		{"missing root", strings.Replace(pvcStorageYAML, "  basePath: /\n", "", 1), "PVC base path"},
		{"local mount is not root", strings.Replace(pvcStorageYAML, "basePath: /", "basePath: /checkpoints", 1), "only the PVC root"},
		{"traversal", strings.Replace(pvcStorageYAML, "basePath: /", "basePath: /a/..", 1), "parent traversal"},
		{"unknown field", pvcStorageYAML + "unknown: value\n", "decode storage configuration"},
		{"misspelled claim", strings.Replace(pvcStorageYAML, "claimName:", "claim:", 1), "decode storage configuration"},
		{"duplicate key", pvcStorageYAML + "pvc: {}\n", "decode storage configuration"},
		{"malformed YAML", "[", "decode storage configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "storage.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte(tc.yaml), 0o600))
			id, err := configuredStoreID(configPath)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				assert.Empty(t, id)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, expected, id)
			// Parsing again models a fresh manager startup, without cached identity.
			again, err := configuredStoreID(configPath)
			require.NoError(t, err)
			assert.Equal(t, id, again)
		})
	}
}

func TestConfiguredStoreIDMissingFileDoesNotFallBackToLegacy(t *testing.T) {
	id, err := configuredStoreID(filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Empty(t, id)
}
