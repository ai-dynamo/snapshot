// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	CuInterposeMountPath       = "/tmp/snapshot-cuda"
	CuInterposeLibraryPath     = CuInterposeMountPath + "/libcuinterpose.so"
	CuInterposeCoreLibraryPath = CuInterposeMountPath + "/libcuinterpose_core.so"
	CuInterposeLauncherPath    = CuInterposeMountPath + "/cuinterpose-launch"
)

// ParseCuInterposeAnnotation preserves the accepted ParseBool spellings while
// distinguishing an absent annotation from a malformed request.
func ParseCuInterposeAnnotation(annotations map[string]string) (bool, error) {
	value, present := annotations[CuInterposeAnnotation]
	if !present {
		return false, nil
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	if err != nil {
		return false, fmt.Errorf("annotation %s: invalid boolean %q", CuInterposeAnnotation, value)
	}
	return enabled, nil
}
