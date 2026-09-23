// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package cuda

import (
	"errors"
	"fmt"
)

var errDeviceValidationUnsupported = fmt.Errorf("NVIDIA device validation requires Linux: %w", errors.ErrUnsupported)

func validateGPUDevice(path string, minor uint32) error {
	return errDeviceValidationUnsupported
}
