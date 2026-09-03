// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// config.go provides configuration loading for the checkpoint agent.
package main

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/go-logr/logr"
	"gopkg.in/yaml.v3"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// ConfigMapPath is the default path where the ConfigMap is mounted.
const ConfigMapPath = "/etc/snapshot/config.yaml"

// LoadConfig loads the agent configuration from a YAML file.
func LoadConfig(path string) (*types.AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	cfg := &types.AgentConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	cfg.LoadEnvOverrides()
	return cfg, nil
}

// NewSkipCompatCheckFn returns a per-restore read of the node-wide switch off
// the mounted ConfigMap, so an admin who flips it does not have to roll the
// DaemonSet to be heard. Kubernetes projects ConfigMap updates into the mount
// on its own; nothing here watches or polls.
//
// A read that fails keeps the last value it did get: a restore is never failed,
// and never quietly checked differently, because a config read went wrong.
func NewSkipCompatCheckFn(path string, initial bool, log logr.Logger) func() bool {
	var last atomic.Bool
	last.Store(initial)
	return func() bool {
		cfg, err := LoadConfig(path)
		if err != nil {
			log.Error(err, "Failed to re-read the restore compatibility switch; keeping the last known value",
				"skipCompatCheck", last.Load(),
				"path", path,
			)
			return last.Load()
		}
		last.Store(cfg.Restore.SkipCompatCheck)
		return cfg.Restore.SkipCompatCheck
	}
}

// LoadConfigOrDefault loads configuration from a file, falling back to defaults if the file doesn't exist.
func LoadConfigOrDefault(path string) (*types.AgentConfig, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg = &types.AgentConfig{}
			cfg.LoadEnvOverrides()
			return cfg, nil
		}
		return nil, err
	}
	return cfg, nil
}
