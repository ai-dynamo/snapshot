// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"strings"
)

const (
	CuInterposeMountPath    = "/tmp/snapshot-cuda"
	CuInterposeLibraryPath  = CuInterposeMountPath + "/libcuinterpose.so"
	CuInterposeLauncherPath = CuInterposeMountPath + "/cuinterpose-launch"
)

// ParseCuInterposeAnnotation accepts enabled or disabled after trimming whitespace.
// An absent annotation leaves support disabled; other present values are invalid.
func ParseCuInterposeAnnotation(annotations map[string]string) (bool, error) {
	value, present := annotations[CuInterposeAnnotation]
	if !present {
		return false, nil
	}
	switch strings.TrimSpace(value) {
	case "enabled":
		return true, nil
	case "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("annotation %s: expected enabled or disabled, got %q", CuInterposeAnnotation, value)
	}
}
