// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package runtime

import (
	"errors"
	"fmt"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

var errMountsUnsupported = fmt.Errorf("mount operations require Linux: %w", errors.ErrUnsupported)

// ReadMountInfo requires Linux procfs mount information.
func ReadMountInfo(pid int) ([]types.MountInfo, error) {
	return nil, errMountsUnsupported
}

// RemountProcSys requires Linux mount operations.
func RemountProcSys(rw bool) error {
	return errMountsUnsupported
}
