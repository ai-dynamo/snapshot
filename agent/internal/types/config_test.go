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
	if cfg.CUDACheckpoint.TransferBufferCount == nil || cfg.CUDACheckpoint.TransferChunkBytes == nil {
		t.Fatal("Validate() did not populate unset CUDA transfer fields")
	}
	settings := cfg.CUDACheckpoint.TransferSettings()
	if settings.BufferCount != DefaultCUDATransferBufferCount || settings.ChunkBytes != DefaultCUDATransferChunkBytes {
		t.Fatalf("CUDA transfer settings = %+v, want defaults", settings)
	}
	if cfg.CUDACheckpoint.StorageMode != CUDAStorageModeLegacy {
		t.Fatalf("CUDA storage mode = %q, want default %q", cfg.CUDACheckpoint.StorageMode, CUDAStorageModeLegacy)
	}
}

func TestAgentConfigValidateCUDAStorageMode(t *testing.T) {
	for _, test := range []struct {
		name      string
		mode      string
		want      string
		wantError bool
	}{
		{name: "unset defaults legacy", want: CUDAStorageModeLegacy},
		{name: "legacy", mode: CUDAStorageModeLegacy, want: CUDAStorageModeLegacy},
		{name: "posix", mode: CUDAStorageModePOSIX, want: CUDAStorageModePOSIX},
		{name: "normalizes", mode: " POSIX ", want: CUDAStorageModePOSIX},
		{name: "rejects auto", mode: "auto", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := validAgentConfig()
			cfg.CUDACheckpoint.StorageMode = test.mode
			err := cfg.Validate()
			if test.wantError {
				if err == nil {
					t.Fatal("Validate() accepted unsupported CUDA storage mode")
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if cfg.CUDACheckpoint.StorageMode != test.want {
				t.Fatalf("CUDA storage mode = %q, want %q", cfg.CUDACheckpoint.StorageMode, test.want)
			}
		})
	}
}

func TestCUDATransferSettingsValidateRejectsBadChunkBytes(t *testing.T) {
	tests := []struct {
		name  string
		chunk uint64
	}{
		{name: "below minimum", chunk: minCUDATransferChunkBytes - cudaTransferBufferAlignment},
		{name: "misaligned", chunk: minCUDATransferChunkBytes + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (CUDATransferSettings{BufferCount: 1, ChunkBytes: test.chunk}).Validate()
			if err == nil {
				t.Fatalf("Validate accepted chunk size %d", test.chunk)
			}
		})
	}
}
