// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"

	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

func bindArtifactCleanupFlags(flags *flag.FlagSet) *operatortypes.ArtifactCleanupConfig {
	cfg := &operatortypes.ArtifactCleanupConfig{}
	flags.StringVar(&cfg.BasePath, "snapshot-storage-base-path", "", "Base path of the shared snapshot storage PVC")
	flags.DurationVar(&cfg.ScanInterval, "artifact-cleanup-scan-interval", operatortypes.DefaultArtifactScanInterval,
		"Interval between pending finalizer recovery and orphan artifact scans")
	flags.IntVar(&cfg.BatchSize, "artifact-cleanup-batch-size", operatortypes.DefaultArtifactBatchSize,
		"Maximum orphan artifact roots removed per scan")
	flags.IntVar(&cfg.ListAttempts, "artifact-cleanup-list-attempts", operatortypes.DefaultArtifactListAttempts,
		"Maximum complete metadata-list attempts per scan")
	flags.IntVar(&cfg.Workers, "artifact-cleanup-workers", operatortypes.DefaultArtifactWorkers,
		"Concurrent maintenance workers processing delete-content and sweep work items")
	flags.DurationVar(&cfg.TaskTimeout, "artifact-cleanup-task-timeout", operatortypes.DefaultArtifactTaskTimeout,
		"Maximum time one maintenance work item may run before it is abandoned and retried")
	flags.StringVar(&cfg.BackendType, "artifact-cleanup-backend-type", operatortypes.DefaultArtifactBackendType,
		"Configured storage backend maintenance routes work items to, matching storage.type")
	return cfg
}

// bindArtifactCleanupS3Flags binds the S3 maintenance backend's own
// credential settings, which the shared storage.yaml identity fields do not
// cover: the maintenance client authenticates independently of PageBroker.
func bindArtifactCleanupS3Flags(flags *flag.FlagSet) (credentialsPath, caBundlePath *string) {
	credentialsPath = flags.String("artifact-cleanup-s3-credentials-path", "",
		"Path to a shared credentials file for the S3 maintenance backend")
	caBundlePath = flags.String("artifact-cleanup-s3-ca-bundle-path", "",
		"Optional path to a CA bundle for the S3 maintenance backend's endpoint")
	return credentialsPath, caBundlePath
}
