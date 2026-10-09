//go:build snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"testing"
)

func TestBindTestHookFlagsReadsE2EEnvironment(t *testing.T) {
	t.Setenv("SNAPSHOT_E2E_TEST_FAIL_CAPTURE_SOURCE_ANNOTATION", "snapshot-e2e.nvidia.com/fail-capture")

	value := bindTestHookFlags(flag.NewFlagSet("test", flag.ContinueOnError))

	if got, want := *value, "snapshot-e2e.nvidia.com/fail-capture"; got != want {
		t.Fatalf("bindTestHookFlags() = %q, want %q", got, want)
	}
}
