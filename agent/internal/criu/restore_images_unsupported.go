// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package criu

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

var errImageMountUnsupported = fmt.Errorf("CRIU image mounts require Linux: %w", errors.ErrUnsupported)

func prepareRestoreImageDir(checkpointPath, scratchDir string) (string, func() error, error) {
	return "", nil, errImageMountUnsupported
}

func mountFilesImage(checkpointPath, replacementFilesImagePath string) (func() error, error) {
	return nil, errImageMountUnsupported
}

func newRestoreTCPSocket() (int, error) {
	// Platforms without SOCK_CLOEXEC must prevent a concurrent fork from
	// inheriting the socket between its creation and setting close-on-exec.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM, unix.IPPROTO_TCP)
	if err != nil {
		return -1, err
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}
