// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package criu

import (
	"errors"
	"fmt"
	"os"
)

var errMountUnsupported = fmt.Errorf("GPU device mounts require Linux: %w", errors.ErrUnsupported)

func (m *GPUDeviceMounts) RestoreNativePaths(pid int) error {
	if len(m.devices) == 0 {
		return nil
	}
	return errMountUnsupported
}

func openGPUDevice(path string) (*os.File, error) {
	return nil, errMountUnsupported
}

func bindGPUDevice(source, target string) error {
	return errMountUnsupported
}

func detachGPUDevice(path string) error {
	return errMountUnsupported
}
