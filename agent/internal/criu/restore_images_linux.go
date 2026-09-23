// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package criu

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const placeholderMountNamespacePath = "/proc/self/ns/mnt"

func prepareRestoreImageDir(checkpointPath, scratchDir string) (string, func() error, error) {
	// The placeholder mount namespace remains container-specific with shareProcessNamespace.
	var stat unix.Stat_t
	if err := unix.Stat(placeholderMountNamespacePath, &stat); err != nil {
		return "", nil, fmt.Errorf("failed to stat placeholder mount namespace at %s: %w", placeholderMountNamespacePath, err)
	}
	return prepareRestoreImageDirForRestoreID(checkpointPath, stat.Ino, scratchDir)
}

func newRestoreTCPSocket() (int, error) {
	return unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
}
