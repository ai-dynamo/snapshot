// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
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

const pvcStorageYAML = `type: pvc
pvc:
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

func TestMustConfigureStoreIDReturnsID(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "storage.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(pvcStorageYAML), 0o600))
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "legacy"},
		{
			name: "configured",
			args: []string{"--snapshot-storage-config=" + configPath},
			want: "store-v1-57e06c9609c92e973d048143531baadb9271a9571f1d1dbc21af2c6cd13d24c2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setManagerArgs(t, append(tc.args, "--snapshot-storage-base-path=/checkpoints")...)
			cleanupConfig := bindArtifactCleanupFlags(flag.CommandLine)
			assert.False(t, flag.Parsed())
			assert.Equal(t, tc.want, mustConfigureStoreID())
			assert.True(t, flag.Parsed())
			// Parsing inside the storage helper also populates the other startup flags.
			assert.Equal(t, "/checkpoints", cleanupConfig.BasePath)
			require.NoError(t, cleanupConfig.Validate())
		})
	}
}

func TestMustConfigureStoreIDExitsOnInvalidConfiguration(t *testing.T) {
	// Exercise os.Exit in a subprocess so it cannot terminate the test runner.
	if os.Getenv("SNAPSHOT_TEST_STORAGE_CONFIG_EXIT") == "1" {
		ctrl.SetLogger(zap.New())
		setManagerArgs(t, "--snapshot-storage-config="+os.Getenv("SNAPSHOT_TEST_STORAGE_CONFIG_PATH"))
		mustConfigureStoreID()
		os.Exit(0) // Returning instead of exiting must fail the parent assertion.
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), executable,
		"-test.run=^TestMustConfigureStoreIDExitsOnInvalidConfiguration$")
	cmd.Env = append(os.Environ(), "SNAPSHOT_TEST_STORAGE_CONFIG_EXIT=1",
		"SNAPSHOT_TEST_STORAGE_CONFIG_PATH="+filepath.Join(t.TempDir(), "missing.yaml"))
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Contains(t, string(output), "invalid storage configuration")
}

func setManagerArgs(t *testing.T, args ...string) {
	t.Helper()
	originalFlags, originalArgs := flag.CommandLine, os.Args
	t.Cleanup(func() { flag.CommandLine, os.Args = originalFlags, originalArgs })
	flag.CommandLine = flag.NewFlagSet("manager-test", flag.ExitOnError)
	os.Args = append([]string{"manager-test"}, args...)
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
		{"missing backend", "pvc: {}", "storage type must be pvc"},
		{"S3 not enabled", "type: s3", "storage type must be pvc"},
		{"missing identity", "type: pvc", "PVC storage identity is required"},
		{"null identity", "type: pvc\npvc: null", "PVC storage identity is required"},
		{"empty identity", "type: pvc\npvc: {}", "PVC namespace"},
		{"missing namespace", strings.Replace(pvcStorageYAML, "  namespace: snapshot\n", "", 1), "PVC namespace"},
		{"missing claim", strings.Replace(pvcStorageYAML, "  claimName: snapshot-pvc\n", "", 1), "PVC claim name"},
		{"missing root", strings.Replace(pvcStorageYAML, "  basePath: /\n", "", 1), "PVC base path"},
		{"local mount is not root", strings.Replace(pvcStorageYAML, "basePath: /", "basePath: /checkpoints", 1), "only the PVC root"},
		{"traversal", strings.Replace(pvcStorageYAML, "basePath: /", "basePath: /a/..", 1), "parent traversal"},
		{"unknown field", pvcStorageYAML + "unknown: value\n", "decode storage configuration"},
		{"misspelled claim", strings.Replace(pvcStorageYAML, "claimName:", "claim:", 1), "decode storage configuration"},
		{"duplicate key", pvcStorageYAML + "type: pvc\n", "decode storage configuration"},
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
