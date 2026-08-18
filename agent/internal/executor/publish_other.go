// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package executor

import "syscall"

func renameNoReplace(_, _ string) error {
	return syscall.ENOTSUP
}
