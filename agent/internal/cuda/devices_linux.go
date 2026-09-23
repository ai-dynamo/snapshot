// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cuda

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// NVIDIA's Linux /dev/nvidiaN character devices use major 195. Checking rdev in
// the workload root prevents a same-named file or another device from qualifying.
// See https://docs.kernel.org/admin-guide/devices.html (195 char).
const nvidiaDeviceMajor = 195

func validateGPUDevice(path string, minor uint32) error {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFCHR || unix.Major(stat.Rdev) != nvidiaDeviceMajor || unix.Minor(stat.Rdev) != minor {
		return fmt.Errorf("expected NVIDIA character device %d:%d", nvidiaDeviceMajor, minor)
	}
	return nil
}
