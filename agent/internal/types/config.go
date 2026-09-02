// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package types defines shared data types used across snapshot packages.
package types

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// CheckpointBasePath is the fixed agent-side checkpoint mount. The privileged
// helper independently enforces the same path.
const CheckpointBasePath = "/checkpoints"

// AgentConfig holds the full agent configuration: static checkpoint settings
// from the ConfigMap YAML, plus runtime fields from environment variables.
type AgentConfig struct {
	NodeName       string                 `yaml:"-"`
	Storage        StorageSpec            `yaml:"storage"`
	CUDACheckpoint CUDACheckpointSettings `yaml:"cudaCheckpoint"`
	Overlay        OverlaySettings        `yaml:"overlay"`
	Restore        RestoreSpec            `yaml:"restore"`
	CRIU           CRIUSettings           `yaml:"criu"`
}

// CUDACheckpointSettings holds CUDA CustomStorage transfer settings.
type CUDACheckpointSettings struct {
	StorageMode         string  `yaml:"storageMode"`
	TransferBufferCount *int    `yaml:"transferBufferCount"`
	TransferChunkBytes  *uint64 `yaml:"transferChunkBytes"`
}

// CUDATransferSettings is the validated, concrete transfer configuration used
// by the CUDA helper daemon.
type CUDATransferSettings struct {
	BufferCount int
	ChunkBytes  uint64
}

const (
	// CUDAStorageModeLegacy uses the CUDA driver's original in-memory storage.
	CUDAStorageModeLegacy = "legacy"
	// CUDAStorageModePOSIX writes CUDA CustomStorage extents into checkpoint files.
	CUDAStorageModePOSIX = "posix"

	DefaultCUDATransferBufferCount = 1
	DefaultCUDATransferChunkBytes  = 64 * 1024 * 1024
	maxCUDATransferBufferCount     = 8
	minCUDATransferChunkBytes      = 1 * 1024 * 1024
	maxCUDATransferChunkBytes      = 256 * 1024 * 1024
	maxCUDAPinnedBytesPerDevice    = 1 * 1024 * 1024 * 1024
	cudaTransferBufferAlignment    = 4096

	CUDAHelperSocketDirectory = "/run/cuda-checkpoint-helper"
	CUDAHelperSocketPath      = CUDAHelperSocketDirectory + "/helper.sock"
)

func (c CUDACheckpointSettings) TransferSettings() CUDATransferSettings {
	settings := CUDATransferSettings{
		BufferCount: DefaultCUDATransferBufferCount,
		ChunkBytes:  DefaultCUDATransferChunkBytes,
	}
	if c.TransferBufferCount != nil {
		settings.BufferCount = *c.TransferBufferCount
	}
	if c.TransferChunkBytes != nil {
		settings.ChunkBytes = *c.TransferChunkBytes
	}
	return settings
}

func (c CUDATransferSettings) WithDefaults() CUDATransferSettings {
	settings := c
	if settings.BufferCount == 0 {
		settings.BufferCount = DefaultCUDATransferBufferCount
	}
	if settings.ChunkBytes == 0 {
		settings.ChunkBytes = DefaultCUDATransferChunkBytes
	}
	return settings
}

func (c CUDATransferSettings) Validate() error {
	if c.BufferCount < 1 || c.BufferCount > maxCUDATransferBufferCount {
		return fmt.Errorf("buffer count must be between 1 and %d", maxCUDATransferBufferCount)
	}
	if c.ChunkBytes < minCUDATransferChunkBytes || c.ChunkBytes > maxCUDATransferChunkBytes || c.ChunkBytes%cudaTransferBufferAlignment != 0 {
		return fmt.Errorf(
			"chunk bytes must be a %d-byte multiple between %d and %d",
			cudaTransferBufferAlignment,
			minCUDATransferChunkBytes,
			maxCUDATransferChunkBytes,
		)
	}
	if uint64(c.BufferCount) > maxCUDAPinnedBytesPerDevice/c.ChunkBytes {
		return fmt.Errorf("buffers exceed the 1 GiB per-device pinned-memory limit")
	}
	return nil
}

func (c *AgentConfig) LoadEnvOverrides() {
	if v := os.Getenv("NODE_NAME"); v != "" {
		c.NodeName = v
	}
}

func (c *AgentConfig) Validate() error {
	storageType := strings.TrimSpace(c.Storage.Type)
	if storageType == "" {
		storageType = "pvc"
	}
	if storageType != "pvc" {
		return &ConfigError{Field: "storage.type", Message: fmt.Sprintf("unsupported storage type %q; only pvc is implemented today", storageType)}
	}
	basePath := c.Storage.BasePath
	if basePath != CheckpointBasePath {
		return &ConfigError{Field: "storage.basePath", Message: fmt.Sprintf("storage.basePath must be %q", CheckpointBasePath)}
	}
	c.Storage.BasePath = basePath
	if c.CRIU.TcpClose && c.CRIU.TcpEstablished {
		return &ConfigError{
			Field:   "criu",
			Message: "tcpClose and tcpEstablished cannot both be true",
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.CRIU.ImageIoMode)) {
	case "", "writeback", "direct":
	default:
		return &ConfigError{
			Field:   "criu.imageIoMode",
			Message: fmt.Sprintf("unsupported imageIoMode %q; expected %q, %q, or empty", c.CRIU.ImageIoMode, "writeback", "direct"),
		}
	}
	if c.CUDACheckpoint.TransferBufferCount == nil {
		value := DefaultCUDATransferBufferCount
		c.CUDACheckpoint.TransferBufferCount = &value
	}
	if c.CUDACheckpoint.TransferChunkBytes == nil {
		value := uint64(DefaultCUDATransferChunkBytes)
		c.CUDACheckpoint.TransferChunkBytes = &value
	}
	if err := c.CUDACheckpoint.TransferSettings().Validate(); err != nil {
		return &ConfigError{Field: "cudaCheckpoint", Message: err.Error()}
	}
	storageMode := strings.ToLower(strings.TrimSpace(c.CUDACheckpoint.StorageMode))
	if storageMode == "" {
		storageMode = CUDAStorageModeLegacy
	}
	switch storageMode {
	case CUDAStorageModeLegacy, CUDAStorageModePOSIX:
		c.CUDACheckpoint.StorageMode = storageMode
	default:
		return &ConfigError{
			Field: "cudaCheckpoint.storageMode",
			Message: fmt.Sprintf(
				"unsupported CUDA storage mode %q; expected %q or %q",
				c.CUDACheckpoint.StorageMode,
				CUDAStorageModeLegacy,
				CUDAStorageModePOSIX,
			),
		}
	}
	return c.Restore.Validate()
}

// StorageSpec holds snapshot storage settings that are local to the agent deployment.
type StorageSpec struct {
	Type     string `yaml:"type"`
	BasePath string `yaml:"basePath"`
}

// RestoreSpec holds settings for the CRIU restore process.
type RestoreSpec struct {
	RestoreTimeoutSeconds int `yaml:"restoreTimeoutSeconds"`
}

func (c *RestoreSpec) RestoreTimeout() time.Duration {
	if c.RestoreTimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.RestoreTimeoutSeconds) * time.Second
}

func (c *RestoreSpec) Validate() error {
	if c.RestoreTimeoutSeconds <= 0 {
		return &ConfigError{Field: "restoreTimeoutSeconds", Message: "restoreTimeoutSeconds must be greater than zero"}
	}
	return nil
}

// CRIUSettings holds CRIU-specific configuration options.
type CRIUSettings struct {
	GhostLimit        uint32 `yaml:"ghostLimit"`
	LogLevel          int32  `yaml:"logLevel"`
	WorkDir           string `yaml:"workDir"`
	AutoDedup         bool   `yaml:"autoDedup"`
	LazyPages         bool   `yaml:"lazyPages"`
	ShellJob          bool   `yaml:"shellJob"`
	TcpClose          bool   `yaml:"tcpClose"`
	TcpEstablished    bool   `yaml:"tcpEstablished"`
	FileLocks         bool   `yaml:"fileLocks"`
	OrphanPtsMaster   bool   `yaml:"orphanPtsMaster"`
	ExtUnixSk         bool   `yaml:"extUnixSk"`
	LinkRemap         bool   `yaml:"linkRemap"`
	ExtMasters        bool   `yaml:"extMasters"`
	ManageCgroupsMode string `yaml:"manageCgroupsMode"`
	ImageIoMode       string `yaml:"imageIoMode"`
	RstSibling        bool   `yaml:"rstSibling"`
	MntnsCompatMode   bool   `yaml:"mntnsCompatMode"`
	EvasiveDevices    bool   `yaml:"evasiveDevices"`
	ForceIrmap        bool   `yaml:"forceIrmap"`
	BinaryPath        string `yaml:"binaryPath"`
	LibDir            string `yaml:"libDir"`
	AllowUprobes      bool   `yaml:"allowUprobes"`
	SkipInFlight      bool   `yaml:"skipInFlight"`
}

// OverlaySettings is the static config for rootfs exclusions.
type OverlaySettings struct {
	Exclusions []string `yaml:"exclusions"`
}

// ConfigError represents a configuration validation error.
type ConfigError struct {
	Field   string
	Message string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("config error: %s: %s", e.Field, e.Message)
}
