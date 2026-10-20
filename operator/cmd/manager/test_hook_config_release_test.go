//go:build !snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"strings"
	"testing"
)

func TestBindTestHookFlagsIgnoresE2EEnvironmentInReleaseBuild(t *testing.T) {
	t.Setenv("SNAPSHOT_E2E_TEST_FAIL_CAPTURE_SOURCE_ANNOTATION", "snapshot-e2e.nvidia.com/fail-capture")

	_, err := bindTestHookFlags(flag.NewFlagSet("test", flag.ContinueOnError))

	if err == nil {
		t.Fatal("bindTestHookFlags() error = nil, want explicit snapshot_e2e build diagnostic")
	}
	if got, want := err.Error(), "requires an operator built with -tags snapshot_e2e"; !strings.Contains(got, want) {
		t.Fatalf("bindTestHookFlags() error = %q, want substring %q", got, want)
	}
}
