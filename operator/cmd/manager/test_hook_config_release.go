//go:build !snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "flag"

func bindTestHookFlags(*flag.FlagSet) *string {
	value := ""
	return &value
}
