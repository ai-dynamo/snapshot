// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"

	"github.com/ai-dynamo/snapshot/api/storage"
)

// mustConfigureStoreID returns the configured store ID.
// Call it once at startup, after registering all other flags: it parses the
// process command line before loading storage configuration.
// On invalid configuration, it logs the error and exits with status 1.
func mustConfigureStoreID() string {
	var storageConfigPath string
	flag.StringVar(&storageConfigPath, "snapshot-storage-config", "", "Path to resolved storage configuration shared with PageBroker")
	flag.Parse()
	storeID, err := configuredStoreID(storageConfigPath)
	if err != nil {
		ctrl.Log.Error(err, "invalid storage configuration")
		os.Exit(1)
	}
	return storeID
}

// configuredStoreID validates the deployment's resolved backend configuration.
// Helm supplies it today; a future storage-class resolver can supply the same
// inputs without exposing backend-specific settings in the agent's API.
func configuredStoreID(configPath string) (string, error) {
	// Older deployments supply only snapshot-storage-base-path. Keep that mode
	// until the producer starts binding new content to its configured store.
	if configPath == "" {
		return "", nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("read storage configuration: %w", err)
	}
	var cfg struct {
		Type string       `json:"type"`
		PVC  *storage.PVC `json:"pvc"`
	}
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return "", fmt.Errorf("decode storage configuration: %w", err)
	}
	if cfg.Type != "pvc" {
		return "", fmt.Errorf("storage type must be pvc; other backends are not supported yet")
	}
	if cfg.PVC == nil {
		return "", fmt.Errorf("PVC storage identity is required")
	}
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
}
