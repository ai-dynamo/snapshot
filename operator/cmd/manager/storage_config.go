// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"

	"github.com/ai-dynamo/snapshot/api/storage"
)

// mustStorageConfig reads configPath and returns its decoded form alongside
// the store ID it configures. On invalid configuration, it logs the error
// and exits with status 1.
func mustStorageConfig(configPath string) (storage.Config, string) {
	cfg, err := loadStorageConfig(configPath)
	if err != nil {
		ctrl.Log.Error(err, "invalid storage configuration")
		os.Exit(1)
	}
	storeID, err := configuredStoreID(cfg, configPath)
	if err != nil {
		ctrl.Log.Error(err, "invalid storage configuration")
		os.Exit(1)
	}
	return cfg, storeID
}

// Store identity remains available to maintenance while new captures stay legacy during rollout.
func newContentStoreID(storeID string, enabled bool) (string, error) {
	if !enabled {
		return "", nil
	}
	if storeID == "" {
		return "", fmt.Errorf("bind-new-contents requires snapshot-storage-config")
	}
	return storeID, nil
}

// loadStorageConfig reads and decodes the chart-rendered storage.yaml.
// configPath == "" is the legacy mode: older deployments supply only
// snapshot-storage-base-path and no resolved backend configuration at all.
func loadStorageConfig(configPath string) (storage.Config, error) {
	if configPath == "" {
		return storage.Config{}, nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return storage.Config{}, fmt.Errorf("read storage configuration: %w", err)
	}
	var cfg storage.Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return storage.Config{}, fmt.Errorf("decode storage configuration: %w", err)
	}
	return cfg, nil
}

// configuredStoreID validates the deployment's resolved backend configuration
// and returns the store identity for whichever single backend it configures.
// Helm supplies it today; a future storage-class resolver can supply the same
// inputs without exposing backend-specific settings in the agent's API.
func configuredStoreID(cfg storage.Config, configPath string) (string, error) {
	if configPath == "" {
		return "", nil
	}
	switch {
	case cfg.PVC != nil && cfg.S3 != nil:
		return "", fmt.Errorf("storage configuration must select exactly one backend, found both pvc and s3")
	case cfg.PVC != nil:
		storeID, err := cfg.PVC.StoreID()
		if err != nil {
			return "", err
		}
		basePath, err := cfg.PVC.NormalizedBasePath()
		if err != nil {
			return "", err
		}
		// The current deployment mounts and uses the whole claim. Accepting a
		// subdirectory would identify a location its filesystem callers do not use.
		if basePath != storage.PVCRoot {
			return "", fmt.Errorf("only the PVC root %q is currently supported", storage.PVCRoot)
		}
		return storeID, nil
	case cfg.S3 != nil:
		return cfg.S3.StoreID()
	default:
		return "", fmt.Errorf("storage configuration must select a backend (pvc or s3)")
	}
}
