// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func validAgentConfig() *AgentConfig {
	return &AgentConfig{
		Storage: StorageSpec{
			Type:     "pvc",
			BasePath: "/checkpoints",
		},
		Restore: RestoreSpec{
			RestoreTimeoutSeconds: 60,
		},
	}
}

// The key an admin flips in the ConfigMap has to be the key the agent reads,
// and an agent whose ConfigMap predates it has to keep the gate on.
func TestRestoreSpecParsesSkipCompatCheck(t *testing.T) {
	cases := map[string]bool{
		"restore:\n  skipCompatCheck: true\n":     true,
		"restore:\n  skipCompatCheck: false\n":    false,
		"restore:\n  restoreTimeoutSeconds: 60\n": false,
	}
	for document, want := range cases {
		cfg := &AgentConfig{}
		if err := yaml.Unmarshal([]byte(document), cfg); err != nil {
			t.Fatalf("unmarshal %q: %v", document, err)
		}
		if cfg.Restore.SkipCompatCheck != want {
			t.Errorf("%q parsed skipCompatCheck = %v, want %v", document, cfg.Restore.SkipCompatCheck, want)
		}
	}
}

func TestAgentConfigValidateRequiresFixedStorageBasePath(t *testing.T) {
	for _, basePath := range []string{"checkpoints", " /checkpoints ", "/checkpoints/../other", "/other"} {
		cfg := validAgentConfig()
		cfg.Storage.BasePath = basePath
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted storage base path %q", basePath)
		}
	}
}

func TestAgentConfigValidateRequiresPageBrokerControlSocket(t *testing.T) {
	cfg := validAgentConfig()
	cfg.PageBroker.Enabled = true

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing PageBroker control socket")
	}
}

func TestCUDACheckpointConfiguration(t *testing.T) {
	for _, mode := range []string{"", "driver", "custom", "invalid"} {
		for _, enabled := range []bool{false, true} {
			for _, checksum := range []bool{false, true} {
				cfg := validAgentConfig()
				cfg.CUDACheckpoint = CUDACheckpointSpec{Enabled: enabled, StorageMode: mode, EnableChecksumDigest: checksum}
				valid := mode != "invalid" && (!checksum || enabled)
				if err := cfg.Validate(); (err == nil) != valid {
					t.Errorf("mode=%q enabled=%t checksum=%t: %v", mode, enabled, checksum, err)
				}
			}
		}
	}
	cfg := validAgentConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.CUDACheckpoint.Enabled || cfg.CUDACheckpoint.EnableChecksumDigest || cfg.CUDACheckpoint.StorageMode != "driver" ||
		cfg.CUDACheckpoint.TransferBufferCount != 32 || cfg.CUDACheckpoint.TransferChunkBytes != 134217728 || cfg.CUDACheckpoint.MaxPinnedBytes != 0 {
		t.Fatalf("unexpected defaults: %+v", cfg.CUDACheckpoint)
	}
	cfg.CUDACheckpoint.TransferBufferCount = ^uint64(0)
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted overflowing pinned buffer allocation")
	}
}
