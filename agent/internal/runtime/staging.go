// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// StagingCapacity reports the total size of the staging volume.
//
// Deliberately the total rather than what is free: free space moves as other
// captures on the node take and release it, so a shortfall against it says only
// that the node is busy. Exceeding the total is a property of this image alone,
// and it is the only thing that can never change.
func StagingCapacity(path string) (total int64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return int64(stat.Blocks) * stat.Bsize, nil
}

// StagingShortfall reports how far an image exceeds what staging could ever
// hold, or zero when it does not provably exceed it.
func StagingShortfall(minimum, total int64) int64 {
	if minimum > total {
		return minimum - total
	}
	return 0
}
