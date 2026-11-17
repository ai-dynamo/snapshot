// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
)

// MemoryLimitEnv holds the container's memory limit in bytes. The chart supplies
// it through resourceFieldRef when the container has a memory limit.
const MemoryLimitEnv = "SNAPSHOT_COORDINATOR_MEMORY_LIMIT_BYTES"

// setMemoryLimit leaves headroom for memory outside Go's runtime. An absent or
// zero limit leaves the runtime's current setting untouched.
func setMemoryLimit() error {
	value := os.Getenv(MemoryLimitEnv)
	if value == "" {
		return nil
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil || limit < 0 {
		return fmt.Errorf("%s must be a non-negative byte count: %q", MemoryLimitEnv, value)
	}
	if limit > 0 {
		debug.SetMemoryLimit(limit/10*9 + limit%10*9/10)
	}
	return nil
}
