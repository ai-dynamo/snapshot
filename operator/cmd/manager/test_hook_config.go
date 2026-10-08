//go:build snapshot_e2e

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "flag"

// bindTestHookFlags registers test-only failures. The chart leaves this empty;
// CPU conformance tests opt in with an exact source-pod annotation key.
func bindTestHookFlags(flags *flag.FlagSet) *string {
	return flags.String("test-fail-capture-source-annotation", "", "test-only source pod annotation requesting PodSnapshot capture failure")
}
