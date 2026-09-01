// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package types defines private runtime configuration shared by the operator.
package types

import (
	"fmt"
	"time"
)

const (
	DefaultArtifactScanInterval = 10 * time.Minute
	DefaultArtifactBatchSize    = 10
	DefaultArtifactListAttempts = 3
)

// ArtifactCleanupConfig configures content finalizer cleanup and orphan scans.
type ArtifactCleanupConfig struct {
	BasePath     string
	ScanInterval time.Duration
	BatchSize    int
	ListAttempts int
}

func (c ArtifactCleanupConfig) Validate() error {
	if c.BasePath == "" {
		return fmt.Errorf("snapshot storage base path is required")
	}
	if c.ScanInterval <= 0 {
		return fmt.Errorf("artifact scan interval must be positive")
	}
	if c.BatchSize <= 0 || c.ListAttempts <= 0 {
		return fmt.Errorf("artifact cleanup integer limits must be positive")
	}
	return nil
}
