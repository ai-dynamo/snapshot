// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func validArtifactCleanupConfig() ArtifactCleanupConfig {
	return ArtifactCleanupConfig{
		BasePath: "/checkpoints", ScanInterval: time.Minute, BatchSize: 1, ListAttempts: 1, Workers: 1, BackendType: "pvc",
	}
}

func TestArtifactCleanupConfigValidatePVCRequiresBasePath(t *testing.T) {
	cfg := validArtifactCleanupConfig()
	cfg.BasePath = ""
	require.ErrorContains(t, cfg.Validate(), "snapshot storage base path is required")
}

func TestArtifactCleanupConfigValidateS3DoesNotRequireBasePath(t *testing.T) {
	cfg := validArtifactCleanupConfig()
	cfg.BasePath = ""
	cfg.BackendType = "s3"
	cfg.S3 = &S3Config{Bucket: "checkpoints", Region: "us-east-1", CredentialsPath: "/etc/snapshot/s3-credentials/credentials"}
	require.NoError(t, cfg.Validate())
}

func TestArtifactCleanupConfigValidateS3RequiresS3Config(t *testing.T) {
	cfg := validArtifactCleanupConfig()
	cfg.BackendType = "s3"
	require.ErrorContains(t, cfg.Validate(), "artifact cleanup s3 config is required")
}
