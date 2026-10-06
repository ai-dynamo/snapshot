// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ResolveHostPIDs maps PIDs in the caller's namespace to host PIDs. procRoot must
// refer to the host proc mount, retained by nsrestore before namespace entry.
// Only the existing host/container PID layout is supported.
func ResolveHostPIDs(procRoot string, pids []int) ([]int, error) {
	namespace, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return nil, fmt.Errorf("stat current PID namespace: %w", err)
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read host proc: %w", err)
	}
	wanted := make(map[int]int, len(pids))
	for _, pid := range pids {
		if pid <= 0 {
			return nil, fmt.Errorf("invalid GPU target PID %d", pid)
		}
		if _, exists := wanted[pid]; exists {
			return nil, fmt.Errorf("duplicate GPU target PID %d", pid)
		}
		wanted[pid] = 0
	}
	for _, entry := range entries {
		hostPID, err := strconv.Atoi(entry.Name())
		if err != nil || hostPID <= 0 {
			continue
		}
		candidate, err := os.Stat(filepath.Join(procRoot, entry.Name(), "ns/pid"))
		if err != nil || !os.SameFile(namespace, candidate) {
			continue // Another namespace or a process that has exited.
		}
		process, err := ReadProcessDetails(procRoot, hostPID)
		if err != nil {
			continue
		}
		if len(process.NamespacePIDs) != 2 || process.NamespacePIDs[0] != hostPID {
			return nil, fmt.Errorf("process %d requires the host/container PID layout", hostPID)
		}
		pid := process.InnermostPID
		previous, match := wanted[pid]
		if !match {
			continue
		}
		if previous != 0 {
			return nil, fmt.Errorf("ambiguous GPU target PID %d", pid)
		}
		wanted[pid] = hostPID
	}
	hostPIDs := make([]int, len(pids))
	for i, pid := range pids {
		if wanted[pid] == 0 {
			return nil, fmt.Errorf("GPU target PID %d not found in host proc", pid)
		}
		hostPIDs[i] = wanted[pid]
	}
	return hostPIDs, nil
}
