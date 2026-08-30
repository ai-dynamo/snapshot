// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	ArtifactStorageBasePathEnv = "SNAPSHOT_STORAGE_BASE_PATH"
	ArtifactScanIntervalEnv    = "SNAPSHOT_ARTIFACT_CLEANUP_SCAN_INTERVAL"
	ArtifactOrphanGraceEnv     = "SNAPSHOT_ARTIFACT_CLEANUP_ORPHAN_GRACE_PERIOD"
	ArtifactBatchSizeEnv       = "SNAPSHOT_ARTIFACT_CLEANUP_BATCH_SIZE"
	ArtifactPageLimitEnv       = "SNAPSHOT_ARTIFACT_CLEANUP_PAGE_LIMIT"
	ArtifactListAttemptsEnv    = "SNAPSHOT_ARTIFACT_CLEANUP_LIST_ATTEMPTS"

	defaultArtifactScanInterval = 10 * time.Minute
	defaultArtifactOrphanGrace  = 5 * time.Minute
	defaultArtifactBatchSize    = 10
	defaultArtifactPageLimit    = 500
	defaultArtifactListAttempts = 3
)

// ArtifactCleanupConfig configures finalizer cleanup and the orphan safety-net scanner.
type ArtifactCleanupConfig struct {
	BasePath     string
	ScanInterval time.Duration
	OrphanGrace  time.Duration
	BatchSize    int
	PageLimit    int64
	ListAttempts int
}

func LoadArtifactCleanupConfigFromEnv() (ArtifactCleanupConfig, error) {
	cfg := ArtifactCleanupConfig{
		BasePath:     os.Getenv(ArtifactStorageBasePathEnv),
		ScanInterval: defaultArtifactScanInterval,
		OrphanGrace:  defaultArtifactOrphanGrace,
		BatchSize:    defaultArtifactBatchSize,
		PageLimit:    defaultArtifactPageLimit,
		ListAttempts: defaultArtifactListAttempts,
	}
	var err error
	if cfg.ScanInterval, err = durationEnv(ArtifactScanIntervalEnv, cfg.ScanInterval); err != nil {
		return ArtifactCleanupConfig{}, err
	}
	if cfg.OrphanGrace, err = durationEnv(ArtifactOrphanGraceEnv, cfg.OrphanGrace); err != nil {
		return ArtifactCleanupConfig{}, err
	}
	if cfg.BatchSize, err = positiveIntEnv(ArtifactBatchSizeEnv, cfg.BatchSize); err != nil {
		return ArtifactCleanupConfig{}, err
	}
	pageLimit, err := positiveIntEnv(ArtifactPageLimitEnv, int(cfg.PageLimit))
	if err != nil {
		return ArtifactCleanupConfig{}, err
	}
	cfg.PageLimit = int64(pageLimit)
	if cfg.ListAttempts, err = positiveIntEnv(ArtifactListAttemptsEnv, cfg.ListAttempts); err != nil {
		return ArtifactCleanupConfig{}, err
	}
	if err := cfg.Validate(); err != nil {
		return ArtifactCleanupConfig{}, err
	}
	return cfg, nil
}

func (c ArtifactCleanupConfig) Validate() error {
	if c.BasePath == "" {
		return fmt.Errorf("%s is required", ArtifactStorageBasePathEnv)
	}
	if c.ScanInterval <= 0 {
		return fmt.Errorf("artifact scan interval must be positive")
	}
	if c.OrphanGrace <= 0 {
		return fmt.Errorf("artifact orphan grace period must be positive")
	}
	if c.BatchSize <= 0 || c.PageLimit <= 0 || c.ListAttempts <= 0 {
		return fmt.Errorf("artifact cleanup integer limits must be positive")
	}
	return nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration: %q", name, value)
	}
	return duration, nil
}

func positiveIntEnv(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer: %q", name, value)
	}
	return parsed, nil
}
