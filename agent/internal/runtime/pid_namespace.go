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

// ReadProcessPIDInNamespace returns the process PID in the given namespace.
// It checks parent namespaces because sibling namespaces can reuse PID values.
func ReadProcessPIDInNamespace(procRoot string, pid int, namespace *os.File) (int, error) {
	if namespace == nil {
		return 0, fmt.Errorf("PID namespace is required")
	}
	kind, err := unix.IoctlRetInt(int(namespace.Fd()), unix.NS_GET_NSTYPE)
	if err != nil || kind != unix.CLONE_NEWPID {
		return 0, fmt.Errorf("descriptor is not a PID namespace")
	}
	pinned, err := namespace.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat pinned PID namespace: %w", err)
	}
	candidate, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "ns/pid"))
	if err != nil {
		return 0, fmt.Errorf("open process PID namespace: %w", err)
	}
	defer func() { _ = candidate.Close() }()
	depth := 0
	for {
		identity, err := candidate.Stat()
		if err != nil {
			return 0, fmt.Errorf("stat process PID namespace: %w", err)
		}
		if os.SameFile(pinned, identity) {
			break
		}
		parent, err := unix.IoctlRetInt(int(candidate.Fd()), unix.NS_GET_PARENT)
		if err != nil {
			return 0, fmt.Errorf("process %d is outside pinned PID namespace: %w", pid, err)
		}
		unix.CloseOnExec(parent)
		_ = candidate.Close()
		candidate = os.NewFile(uintptr(parent), "parent-pid-namespace")
		depth++
	}
	process, err := ReadProcessDetails(procRoot, pid)
	if err != nil {
		return 0, err
	}
	return pidAtNamespaceDepth(process.NamespacePIDs, depth)
}

func pidAtNamespaceDepth(pids []int, depth int) (int, error) {
	if depth < 0 || depth >= len(pids) || pids[len(pids)-depth-1] <= 0 {
		return 0, fmt.Errorf("process PID namespace ancestry does not match NSpid")
	}
	return pids[len(pids)-depth-1], nil
}

// ResolveHostPIDs maps PIDs in the caller's namespace to host PIDs. procRoot must
// refer to the host proc mount, retained by nsrestore before namespace entry.
func ResolveHostPIDs(procRoot string, pids []int) ([]int, error) {
	namespace, err := os.Open("/proc/self/ns/pid")
	if err != nil {
		return nil, fmt.Errorf("open current PID namespace: %w", err)
	}
	defer namespace.Close()
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
		pid, err := ReadProcessPIDInNamespace(procRoot, hostPID, namespace)
		if err != nil {
			continue // Processes outside this namespace or already gone.
		}
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
