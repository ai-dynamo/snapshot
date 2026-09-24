// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package cuda

import (
	"errors"
	"testing"
)

func TestValidateGPUDeviceUnsupported(t *testing.T) {
	if err := validateGPUDevice("/dev/nvidia0", 0); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("validateGPUDevice = %v; want unsupported", err)
	}
	paths, err := ResolveDevicePaths(t.TempDir(), 1, nil)
	if err != nil || paths != nil {
		t.Fatalf("empty GPU selection = %v, %v; want nil, nil", paths, err)
	}
	paths, err = ResolveDevicePaths(t.TempDir(), 1, []string{"GPU-A"})
	if !errors.Is(err, errors.ErrUnsupported) || paths != nil {
		t.Fatalf("nonempty GPU selection = %v, %v; want nil, unsupported", paths, err)
	}
}
