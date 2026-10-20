//go:build !snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"os"
)

func bindTestHookFlags(*flag.FlagSet) (*string, error) {
	if value := os.Getenv("SNAPSHOT_E2E_TEST_FAIL_CAPTURE_SOURCE_ANNOTATION"); value != "" {
		return nil, fmt.Errorf(
			"SNAPSHOT_E2E_TEST_FAIL_CAPTURE_SOURCE_ANNOTATION requires an operator built with -tags snapshot_e2e",
		)
	}
	value := ""
	return &value, nil
}
