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

func TestCUDATransferSettingsWithDefaults(t *testing.T) {
	got := (CUDATransferSettings{}).WithDefaults()
	if got.BufferCount != DefaultCUDATransferBufferCount || got.ChunkBytes != DefaultCUDATransferChunkBytes {
		t.Fatalf("WithDefaults() = %+v, want 1 slot and 64 MiB", got)
	}
}

func TestAgentConfigValidateCUDATransferSettings(t *testing.T) {
	cfg := validAgentConfig()
	bufferCount := 4
	chunkBytes := uint64(32 * 1024 * 1024)
	cfg.CUDACheckpoint.TransferBufferCount = &bufferCount
	cfg.CUDACheckpoint.TransferChunkBytes = &chunkBytes
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := cfg.CUDACheckpoint.TransferSettings(); got.BufferCount != bufferCount || got.ChunkBytes != chunkBytes {
		t.Fatalf("CUDA transfer settings = %+v, want count=%d chunk=%d", got, bufferCount, chunkBytes)
	}

	tooManyBuffers := 8
	tooLargeChunk := uint64(256 * 1024 * 1024)
	cfg.CUDACheckpoint.TransferBufferCount = &tooManyBuffers
	cfg.CUDACheckpoint.TransferChunkBytes = &tooLargeChunk
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected excessive per-device pinned memory to be rejected")
	}
}

func TestAgentConfigValidateDefaultsCUDATransferSettings(t *testing.T) {
	cfg := validAgentConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	settings := cfg.CUDACheckpoint.TransferSettings()
	if settings.BufferCount != DefaultCUDATransferBufferCount || settings.ChunkBytes != DefaultCUDATransferChunkBytes {
		t.Fatalf("CUDA transfer settings = %+v, want defaults", settings)
	}
}
