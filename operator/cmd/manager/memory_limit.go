// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"runtime/debug"
	"strconv"
)

// memoryLimitEnv holds the manager container's memory limit in bytes. The chart
// sets it through a resourceFieldRef when the values set a memory limit.
const memoryLimitEnv = "SNAPSHOT_OPERATOR_MEMORY_LIMIT_BYTES"

// setMemoryLimit gives the Go runtime 90% of the container's memory limit as
// its soft limit and returns it. The remaining tenth is for memory the runtime
// does not count, such as the pages of the binary. An empty or zero value
// leaves the runtime's setting alone and returns 0.
func setMemoryLimit(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	containerLimit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || containerLimit < 0 {
		return 0, fmt.Errorf("%s must be a non-negative byte count: %q", memoryLimitEnv, value)
	}
	if containerLimit == 0 {
		return 0, nil
	}
	// Nine tenths without overflowing near the largest int64.
	limit := containerLimit/10*9 + containerLimit%10*9/10
	debug.SetMemoryLimit(limit)
	return limit, nil
}
