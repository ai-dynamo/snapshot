// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"

	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

// bindArtifactCleanupFlags binds cfg.S3 to a non-nil, possibly-empty S3Config;
// finalizeArtifactCleanupFlags must run after flag.Parse to clear it when unused.
func bindArtifactCleanupFlags(flags *flag.FlagSet) *operatortypes.ArtifactCleanupConfig {
	cfg := &operatortypes.ArtifactCleanupConfig{S3: &operatortypes.S3Config{}}
	flags.StringVar(&cfg.BasePath, "snapshot-storage-base-path", "", "Base path of the shared snapshot storage PVC")
	flags.DurationVar(&cfg.ScanInterval, "artifact-cleanup-scan-interval", operatortypes.DefaultArtifactScanInterval,
		"Interval between pending finalizer recovery and orphan artifact scans")
	flags.IntVar(&cfg.BatchSize, "artifact-cleanup-batch-size", operatortypes.DefaultArtifactBatchSize,
		"Maximum orphan artifact roots removed per scan")
	flags.IntVar(&cfg.ListAttempts, "artifact-cleanup-list-attempts", operatortypes.DefaultArtifactListAttempts,
		"Maximum complete metadata-list attempts per scan")
	flags.IntVar(&cfg.Workers, "artifact-cleanup-workers", operatortypes.DefaultArtifactWorkers,
		"Concurrent maintenance workers processing delete-content and sweep work items")
	flags.StringVar(&cfg.BackendType, "artifact-cleanup-backend-type", operatortypes.DefaultArtifactBackendType,
		"Configured storage backend maintenance routes work items to, matching storage.type")
	flags.StringVar(&cfg.S3.Bucket, "artifact-cleanup-s3-bucket", "", "S3 bucket the S3 maintenance backend targets; unset disables it")
	flags.StringVar(&cfg.S3.Prefix, "artifact-cleanup-s3-prefix", "", "Key prefix under the S3 bucket")
	flags.StringVar(&cfg.S3.Region, "artifact-cleanup-s3-region", "", "S3 bucket region")
	flags.StringVar(&cfg.S3.Endpoint, "artifact-cleanup-s3-endpoint", "", "S3-compatible endpoint override; empty uses the default AWS endpoint")
	flags.StringVar(&cfg.S3.CredentialsPath, "artifact-cleanup-s3-credentials-path", operatortypes.DefaultS3CredentialsPath,
		"Path to the projected S3 shared-credentials file")
	flags.StringVar(&cfg.S3.CABundlePath, "artifact-cleanup-s3-ca-bundle-path", "", "Optional path to a custom CA bundle for the S3 endpoint")
	return cfg
}

// finalizeArtifactCleanupFlags clears cfg.S3 when no bucket was configured,
// so BackendRegistry.Init does not register an unusable S3 backend.
func finalizeArtifactCleanupFlags(cfg *operatortypes.ArtifactCleanupConfig) {
	if cfg.S3.Bucket == "" {
		cfg.S3 = nil
	}
}
