// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

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

func TestAgentConfigValidateRequiresAbsoluteStorageBasePath(t *testing.T) {
	cfg := validAgentConfig()
	cfg.Storage.BasePath = "checkpoints"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for relative storage base path")
	}
}

func TestAgentConfigValidateRejectsWhitespaceInStorageBasePath(t *testing.T) {
	cfg := validAgentConfig()
	cfg.Storage.BasePath = " /checkpoints "

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for storage base path containing whitespace")
	}
}

func TestAgentConfigValidateRejectsUncleanStorageBasePath(t *testing.T) {
	cfg := validAgentConfig()
	cfg.Storage.BasePath = "/checkpoints/../other"

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unclean storage base path")
	}
}

func TestAgentConfigValidateRequiresFixedStorageBasePath(t *testing.T) {
	cfg := validAgentConfig()
	cfg.Storage.BasePath = "/other"

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for non-standard storage base path")
	}
}
