// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// TaskProbe is what /proc says about one task's availability to CRIU.
type TaskProbe struct {
	PID       int
	TracerPID int
	// State is the first field of the status line: R, S, D, T and so on.
	State   string
	Threads int
}

// TreeSnapshot records the tree at suspend as PID to thread count, so a later
// probe can tell whether anything exited or forked since.
type TreeSnapshot map[int]int

// ProbeTask reads the three fields that decide whether CRIU can seize a task.
func ProbeTask(procRoot string, pid int) (TaskProbe, error) {
	if pid <= 0 {
		return TaskProbe{}, fmt.Errorf("invalid PID %d", pid)
	}
	statusPath := filepath.Join(procRoot, strconv.Itoa(pid), "status")
	statusBytes, err := os.ReadFile(statusPath)
	if err != nil {
		return TaskProbe{}, fmt.Errorf("read %s: %w", statusPath, err)
	}
	probe := TaskProbe{PID: pid, TracerPID: -1, Threads: -1}
	for _, line := range strings.Split(string(statusBytes), "\n") {
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch field {
		case "State":
			probe.State, _, _ = strings.Cut(value, " ")
		case "TracerPid":
			if probe.TracerPID, err = strconv.Atoi(value); err != nil {
				return TaskProbe{}, fmt.Errorf("parse TracerPid %q: %w", value, err)
			}
		case "Threads":
			if probe.Threads, err = strconv.Atoi(value); err != nil {
				return TaskProbe{}, fmt.Errorf("parse Threads %q: %w", value, err)
			}
		}
	}
	if probe.State == "" || probe.TracerPID < 0 || probe.Threads < 0 {
		return TaskProbe{}, fmt.Errorf("incomplete status for PID %d", pid)
	}
	return probe, nil
}

// SnapshotTree records the tree so a later probe can compare against it.
func SnapshotTree(procRoot string, pids []int) (TreeSnapshot, error) {
	snapshot := make(TreeSnapshot, len(pids))
	for _, pid := range pids {
		probe, err := ProbeTask(procRoot, pid)
		if err != nil {
			return nil, err
		}
		snapshot[pid] = probe.Threads
	}
	return snapshot, nil
}

// Dumpable reports whether CRIU can dump the recorded tree as it stands, and
// names the first signal that says it cannot.
//
// CRIU seizes every task and injects a parasite, undoing both when it finishes.
// A run that was interrupted can leave a task attached or stopped, and CRIU
// cannot seize a task that is already seized.
func Dumpable(procRoot string, recorded TreeSnapshot) (bool, string) {
	for _, pid := range sortedPIDs(recorded) {
		probe, err := ProbeTask(procRoot, pid)
		if err != nil {
			return false, fmt.Sprintf("process %d is gone or unreadable: %v", pid, err)
		}
		switch {
		case probe.TracerPID != 0:
			return false, fmt.Sprintf("process %d is still traced by %d", pid, probe.TracerPID)
		case probe.State == "T" || probe.State == "t":
			return false, fmt.Sprintf("process %d is stopped", pid)
		case probe.State == "D":
			return false, fmt.Sprintf("process %d is in uninterruptible sleep", pid)
		case probe.Threads != recorded[pid]:
			return false, fmt.Sprintf("process %d now has %d threads, not %d", pid, probe.Threads, recorded[pid])
		}
	}
	return true, ""
}

// GPUStateUniform reports whether every GPU process reads want. A suspend that
// failed partway leaves some merely locked, and that tree is not the one the
// dump was meant to capture.
func GPUStateUniform(states map[int]string, want string) (bool, string) {
	for _, pid := range sortedPIDs(states) {
		if states[pid] != want {
			return false, fmt.Sprintf("process %d reads %q, not %q", pid, states[pid], want)
		}
	}
	return true, ""
}

// sortedPIDs keeps the reported signal stable across runs.
func sortedPIDs[V any](m map[int]V) []int {
	pids := make([]int, 0, len(m))
	for pid := range m {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}
