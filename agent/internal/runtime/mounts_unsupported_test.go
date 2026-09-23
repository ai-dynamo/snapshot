// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package runtime

import (
	"errors"
	"testing"
)

func TestMountOperationsUnsupported(t *testing.T) {
	mounts, err := ReadMountInfo(1)
	if !errors.Is(err, errors.ErrUnsupported) || mounts != nil {
		t.Fatalf("ReadMountInfo = %v, %v; want nil, unsupported", mounts, err)
	}
	for _, rw := range []bool{true, false} {
		if err := RemountProcSys(rw); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("RemountProcSys(%v) = %v; want unsupported", rw, err)
		}
	}
}
