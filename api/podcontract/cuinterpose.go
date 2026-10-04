// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	CuInterposeMountPath    = "/tmp/snapshot-cuda"
	CuInterposeLibraryPath  = CuInterposeMountPath + "/libcuinterpose.so"
	CuInterposeLauncherPath = CuInterposeMountPath + "/cuinterpose-launch"
)

// ParseCuInterposeAnnotation accepts the same values as ParseBool. It distinguishes an
// absent annotation from an invalid value.
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
