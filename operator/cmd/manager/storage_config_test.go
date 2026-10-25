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

const s3StorageYAML = `s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: snapshot-bucket
`

const pvcStoreID = "store-v1-57e06c9609c92e973d048143531baadb9271a9571f1d1dbc21af2c6cd13d24c2"

// storeIDFromPath mirrors how main() derives a store ID from a config path.
func storeIDFromPath(configPath string) (string, error) {
	cfg, err := loadStorageConfig(configPath)
	if err != nil {
		return "", err
	}
	return configuredStoreID(cfg, configPath)
}

func TestNewContentBindingIsAnExplicitRolloutStep(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "storage.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(pvcStorageYAML), 0o600))
	storeID, err := storeIDFromPath(configPath)
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
	id, err := storeIDFromPath("")
	require.NoError(t, err)
	assert.Empty(t, id)
}

func TestMustStorageConfigReturnsID(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "storage.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(pvcStorageYAML), 0o600))
	s3Path := filepath.Join(t.TempDir(), "storage.yaml")
	require.NoError(t, os.WriteFile(s3Path, []byte(s3StorageYAML), 0o600))
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "legacy"},
		{name: "pvc configured", path: configPath, want: pvcStoreID},
		{name: "s3 configured", path: s3Path, want: "store-v1-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, storeID := mustStorageConfig(tc.path)
			if tc.name == "s3 configured" {
				assert.True(t, strings.HasPrefix(storeID, tc.want))
				return
			}
			assert.Equal(t, tc.want, storeID)
		})
	}
}

func TestMustStorageConfigExitsOnInvalidConfiguration(t *testing.T) {
	// Exercise os.Exit in a subprocess so it cannot terminate the test runner.
	if os.Getenv("SNAPSHOT_TEST_STORAGE_CONFIG_EXIT") == "1" {
		ctrl.SetLogger(zap.New())
		mustStorageConfig(os.Getenv("SNAPSHOT_TEST_STORAGE_CONFIG_PATH"))
		os.Exit(0) // Returning instead of exiting must fail the parent assertion.
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), executable,
		"-test.run=^TestMustStorageConfigExitsOnInvalidConfiguration$")
	cmd.Env = append(os.Environ(), "SNAPSHOT_TEST_STORAGE_CONFIG_EXIT=1",
		"SNAPSHOT_TEST_STORAGE_CONFIG_PATH="+filepath.Join(t.TempDir(), "missing.yaml"))
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Contains(t, string(output), "invalid storage configuration")
}

func TestConfiguredStoreID(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
		err  string
	}{
		{name: "PVC identity", yaml: pvcStorageYAML, want: pvcStoreID},
		{name: "normalized root", yaml: strings.Replace(pvcStorageYAML, "basePath: /", "basePath: ///./", 1), want: pvcStoreID},
		{name: "S3 identity", yaml: s3StorageYAML},
		{name: "missing backend", yaml: "{}", err: "must select a backend"},
		{name: "both backends", yaml: pvcStorageYAML + "s3:\n  endpoint: https://s3.example.com\n  region: us-east-1\n  bucket: b\n", err: "exactly one backend"},
		{name: "type discriminator", yaml: "type: pvc\n" + pvcStorageYAML, err: "decode storage configuration"},
		{name: "null identity", yaml: "pvc: null", err: "must select a backend"},
		{name: "empty identity", yaml: "pvc: {}", err: "PVC namespace"},
		{name: "missing namespace", yaml: strings.Replace(pvcStorageYAML, "  namespace: snapshot\n", "", 1), err: "PVC namespace"},
		{name: "missing claim", yaml: strings.Replace(pvcStorageYAML, "  claimName: snapshot-pvc\n", "", 1), err: "PVC claim name"},
		{name: "missing root", yaml: strings.Replace(pvcStorageYAML, "  basePath: /\n", "", 1), err: "PVC base path"},
		{name: "local mount is not root", yaml: strings.Replace(pvcStorageYAML, "basePath: /", "basePath: /checkpoints", 1), err: "only the PVC root"},
		{name: "traversal", yaml: strings.Replace(pvcStorageYAML, "basePath: /", "basePath: /a/..", 1), err: "parent traversal"},
		{name: "unknown field", yaml: pvcStorageYAML + "unknown: value\n", err: "decode storage configuration"},
		{name: "misspelled claim", yaml: strings.Replace(pvcStorageYAML, "claimName:", "claim:", 1), err: "decode storage configuration"},
		{name: "duplicate key", yaml: pvcStorageYAML + "pvc: {}\n", err: "decode storage configuration"},
		{name: "malformed YAML", yaml: "[", err: "decode storage configuration"},
		{name: "invalid S3 bucket", yaml: "s3:\n  endpoint: https://s3.example.com\n  region: us-east-1\n  bucket: AB\n", err: "S3 bucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "storage.yaml")
			require.NoError(t, os.WriteFile(configPath, []byte(tc.yaml), 0o600))
			id, err := storeIDFromPath(configPath)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				assert.Empty(t, id)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, id)
			if tc.want != "" {
				assert.Equal(t, tc.want, id)
			}
			// Parsing again models a fresh manager startup, without cached identity.
			again, err := storeIDFromPath(configPath)
			require.NoError(t, err)
			assert.Equal(t, id, again)
		})
	}
}

func TestConfiguredStoreIDNormalizesS3Prefix(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "storage.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(s3StorageYAML+"  prefix: containers//./\n"), 0o600))
	cfg, err := loadStorageConfig(configPath)
	require.NoError(t, err)
	storeID, err := configuredStoreID(cfg, configPath)
	require.NoError(t, err)
	require.NotEmpty(t, storeID)

	// ensurePageBrokerS3Config and the maintenance backend both read
	// cfg.S3.Prefix directly; it must match what StoreID hashed.
	normalized, err := cfg.S3.NormalizedPrefix()
	require.NoError(t, err)
	assert.Equal(t, "containers", normalized)
	assert.Equal(t, normalized, cfg.S3.Prefix)
}

func TestConfiguredStoreIDMissingFileDoesNotFallBackToLegacy(t *testing.T) {
	id, err := storeIDFromPath(filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Empty(t, id)
}
