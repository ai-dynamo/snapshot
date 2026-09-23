// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBindArtifactCleanupFlagsDefaults(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := bindArtifactCleanupFlags(flags)
	require.NoError(t, flags.Parse([]string{"--snapshot-storage-base-path=/checkpoints"}))
	finalizeArtifactCleanupFlags(cfg)
	require.NoError(t, cfg.Validate())
	assert.Equal(t, 10*time.Minute, cfg.ScanInterval)
	assert.Equal(t, 10, cfg.BatchSize)
	assert.Equal(t, 3, cfg.ListAttempts)
	assert.Nil(t, cfg.S3, "S3 backend must stay disabled when no bucket flag is set")
}

func TestBindArtifactCleanupFlagsKeepsS3ConfigWhenBucketIsSet(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := bindArtifactCleanupFlags(flags)
	require.NoError(t, flags.Parse([]string{
		"--snapshot-storage-base-path=/checkpoints",
		"--artifact-cleanup-s3-bucket=checkpoints",
		"--artifact-cleanup-s3-region=us-east-1",
	}))
	finalizeArtifactCleanupFlags(cfg)
	require.NoError(t, cfg.Validate())
	require.NotNil(t, cfg.S3)
	assert.Equal(t, "checkpoints", cfg.S3.Bucket)
	assert.Equal(t, "us-east-1", cfg.S3.Region)
}

func TestArtifactCleanupFlagsRejectNonPositiveSettings(t *testing.T) {
	for _, tc := range []struct{ name, flag, value string }{
		{"zero interval", "artifact-cleanup-scan-interval", "0s"},
		{"negative interval", "artifact-cleanup-scan-interval", "-1s"},
		{"zero batch", "artifact-cleanup-batch-size", "0"},
		{"negative batch", "artifact-cleanup-batch-size", "-1"},
		{"zero attempts", "artifact-cleanup-list-attempts", "0"},
		{"negative attempts", "artifact-cleanup-list-attempts", "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			cfg := bindArtifactCleanupFlags(flags)
			require.NoError(t, flags.Parse([]string{"--snapshot-storage-base-path=/checkpoints", "--" + tc.flag + "=" + tc.value}))
			finalizeArtifactCleanupFlags(cfg)
			require.Error(t, cfg.Validate())
		})
	}
}
