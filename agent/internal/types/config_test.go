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

func TestAgentConfigValidatePageBrokerTransferEngine(t *testing.T) {
	for _, tc := range []struct {
		configured string
		want       string
		valid      bool
	}{
		{configured: "", want: "posix-copy", valid: true},
		{configured: "posix-copy", want: "posix-copy", valid: true},
		{configured: "model-streamer", want: "model-streamer", valid: true},
		{configured: "unknown", valid: false},
	} {
		cfg := validAgentConfig()
		cfg.PageBroker.TransferEngine = tc.configured
		err := cfg.Validate()
		if tc.valid && err != nil {
			t.Errorf("Validate transfer engine %q: %v", tc.configured, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("Validate accepted transfer engine %q", tc.configured)
		}
		if tc.valid && cfg.PageBroker.TransferEngine != tc.want {
			t.Errorf("transfer engine = %q, want %q", cfg.PageBroker.TransferEngine, tc.want)
		}
	}
}
