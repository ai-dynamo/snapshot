//go:build snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
)

// bindTestHookFlags reads test-only failures. Production binaries ignore the
// environment variable, so a chart value cannot add an unsupported CLI flag.
func bindTestHookFlags(*flag.FlagSet) *string {
	value := os.Getenv("SNAPSHOT_E2E_TEST_FAIL_CAPTURE_SOURCE_ANNOTATION")
	return &value
}
