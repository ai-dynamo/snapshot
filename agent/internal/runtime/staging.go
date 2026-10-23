// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// StagingCapacity reports the bytes the staging volume can still take. Staging
// is a memory-backed emptyDir with a sizeLimit, so this is the cap the chart
// set minus whatever other captures on this node are already holding.
func StagingCapacity(path string) (available int64, err error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return int64(stat.Bavail) * stat.Bsize, nil
}

// EstimateImageSize sums the resident memory of a process tree.
//
// It is a lower bound, not a prediction: CRIU writes more than RSS, and the
// total moves while the workload runs. So it can establish that an image cannot
// possibly fit, never that it will.
func EstimateImageSize(procRoot string, pids []int) (int64, error) {
	var total int64
	for _, pid := range pids {
		rss, err := readVmRSS(procRoot, pid)
		if err != nil {
			return 0, err
		}
		total += rss
	}
	return total, nil
}

// readVmRSS returns one process's resident set size in bytes.
func readVmRSS(procRoot string, pid int) (int64, error) {
	statusPath := filepath.Join(procRoot, strconv.Itoa(pid), "status")
	statusBytes, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", statusPath, err)
	}
	for _, line := range strings.Split(string(statusBytes), "\n") {
		field, value, found := strings.Cut(line, ":")
		if !found || field != "VmRSS" {
			continue
		}
		// VmRSS is reported as "<n> kB"; the kernel always uses kB here.
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return 0, fmt.Errorf("empty VmRSS for PID %d", pid)
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse VmRSS %q for PID %d: %w", fields[0], pid, err)
		}
		return kb * 1024, nil
	}
	// Kernel threads have no VmRSS and contribute nothing to the image.
	return 0, nil
}

// StagingShortfall reports how far the estimate exceeds what staging can take,
// or zero when it does not provably exceed it.
func StagingShortfall(estimate, available int64) int64 {
	if estimate > available {
		return estimate - available
	}
	return 0
}
