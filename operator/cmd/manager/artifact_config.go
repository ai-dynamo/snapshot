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
		"Interval between orphan artifact scans")
	flags.IntVar(&cfg.BatchSize, "artifact-cleanup-batch-size", operatortypes.DefaultArtifactBatchSize,
		"Maximum orphan artifact roots removed per scan")
	flags.IntVar(&cfg.ListAttempts, "artifact-cleanup-list-attempts", operatortypes.DefaultArtifactListAttempts,
		"Maximum complete metadata-list attempts per scan")
	return cfg
}
