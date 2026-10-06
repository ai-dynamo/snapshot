// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// ResolveHostPIDs maps PIDs in the caller's namespace to host PIDs. procRoot must
// refer to the host proc mount, retained by nsrestore before namespace entry.
// CRIU may create a child PID namespace below the restore container.
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
		candidate, err := os.Open(filepath.Join(procRoot, entry.Name(), "ns/pid"))
		if err != nil {
			continue
		}
		identity, err := candidate.Stat()
		depth := 2 // Host and restore container.
		if err == nil && !os.SameFile(namespace, identity) {
			// A restored process can belong to CRIU's child namespace. Its
			// parent must be our container, not another pod's namespace.
			parent, parentErr := unix.IoctlRetInt(int(candidate.Fd()), unix.NS_GET_PARENT)
			if parentErr == nil {
				unix.CloseOnExec(parent)
				_ = candidate.Close()
				candidate = os.NewFile(uintptr(parent), "parent PID namespace")
				identity, err = candidate.Stat()
				depth++
			}
		}
		_ = candidate.Close()
		if err != nil || !os.SameFile(namespace, identity) {
			continue // Another namespace or a process that has exited.
		}
		process, err := ReadProcessDetails(procRoot, hostPID)
		if err != nil {
			continue
		}
		if len(process.NamespacePIDs) != depth || process.NamespacePIDs[0] != hostPID {
			return nil, fmt.Errorf("process %d requires the host/container PID layout", hostPID)
		}
		pid := process.NamespacePIDs[1]
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
