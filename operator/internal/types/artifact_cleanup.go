// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package types defines private runtime configuration shared by the operator.
package types

import (
	"fmt"
	"strings"
	"time"
)

const (
	DefaultArtifactScanInterval = 10 * time.Minute
	DefaultArtifactBatchSize    = 10
	DefaultArtifactListAttempts = 3
	DefaultArtifactWorkers      = 5
	DefaultArtifactBackendType  = "pvc"
)

// ArtifactCleanupConfig configures content finalizer cleanup and orphan scans.
type ArtifactCleanupConfig struct {
	BasePath     string
	ScanInterval time.Duration
	BatchSize    int
	ListAttempts int
	Workers      int
	BackendType  string
	// S3 is nil unless the S3 maintenance backend is configured.
	S3 *S3Config
}

func (c ArtifactCleanupConfig) Validate() error {
	if c.ScanInterval <= 0 {
		return fmt.Errorf("artifact scan interval must be positive")
	}
	if c.BatchSize <= 0 || c.ListAttempts <= 0 {
		return fmt.Errorf("artifact cleanup integer limits must be positive")
	}
	if c.Workers <= 0 {
		return fmt.Errorf("artifact cleanup worker count must be positive")
	}
	if c.BackendType == "" {
		return fmt.Errorf("artifact cleanup backend type is required")
	}
	if strings.EqualFold(c.BackendType, "s3") {
		if c.S3 == nil {
			return fmt.Errorf("artifact cleanup s3 config is required when the backend type is s3")
		}
		if err := c.S3.Validate(); err != nil {
			return fmt.Errorf("artifact cleanup s3 config: %w", err)
		}
		return nil
	}
	if c.BasePath == "" {
		return fmt.Errorf("snapshot storage base path is required")
	}
	return nil
}
