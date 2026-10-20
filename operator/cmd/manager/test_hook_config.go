//go:build snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
)

// bindTestHookFlags reads the test-only failure hook in snapshot_e2e builds.
func bindTestHookFlags(*flag.FlagSet) (*string, error) {
	value := os.Getenv("SNAPSHOT_E2E_TEST_FAIL_CAPTURE_SOURCE_ANNOTATION")
	return &value, nil
}
