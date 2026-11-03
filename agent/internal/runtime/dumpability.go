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

// ProbeTask reads the three fields that decide whether CRIU can seize a task,
// for a process's thread-group leader.
func ProbeTask(procRoot string, pid int) (TaskProbe, error) {
	return probeStatus(procRoot, pid, filepath.Join(procRoot, strconv.Itoa(pid), "status"))
}

// probeTaskOf reads those fields for one task of a process.
func probeTaskOf(procRoot string, pid, tid int) (TaskProbe, error) {
	return probeStatus(procRoot, tid,
		filepath.Join(procRoot, strconv.Itoa(pid), "task", strconv.Itoa(tid), "status"))
}

func probeStatus(procRoot string, pid int, statusPath string) (TaskProbe, error) {
	if pid <= 0 {
		return TaskProbe{}, fmt.Errorf("invalid PID %d", pid)
	}
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

// Released reports whether CRIU has let go of every task, and names the first
// one it has not.
//
// CRIU seizes every task and injects a parasite, undoing both when it finishes.
// A run that was interrupted can leave a task attached or stopped, and such a
// task cannot resume on its own.
//
// Uninterruptible sleep is deliberately not disqualifying here: it ends by
// itself, and a thread sitting in a driver call is ordinary right after a GPU
// restore. Only a dump cares, because CRIU cannot seize such a task.
func Released(procRoot string, pids []int) (bool, string) {
	for _, pid := range pids {
		tids, err := taskIDs(procRoot, pid)
		if err != nil {
			return false, fmt.Sprintf("process %d is gone or unreadable: %v", pid, err)
		}
		for _, tid := range tids {
			probe, err := probeTaskOf(procRoot, pid, tid)
			if err != nil {
				return false, fmt.Sprintf("task %d of process %d is gone or unreadable: %v", tid, pid, err)
			}
			switch {
			case probe.TracerPID != 0:
				return false, fmt.Sprintf("task %d of process %d is still traced by %d", tid, pid, probe.TracerPID)
			case probe.State == "T" || probe.State == "t":
				return false, fmt.Sprintf("task %d of process %d is stopped", tid, pid)
			}
		}
	}
	return true, ""
}

// taskIDs lists every task in a process. CRIU seizes tasks, not processes, so a
// non-leader thread left traced or stopped keeps the tree from resuming even
// when its leader reads clean.
func taskIDs(procRoot string, pid int) ([]int, error) {
	taskDir := filepath.Join(procRoot, strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", taskDir, err)
	}
	tids := make([]int, 0, len(entries))
	for _, entry := range entries {
		tid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		tids = append(tids, tid)
	}
	if len(tids) == 0 {
		return nil, fmt.Errorf("process %d has no tasks", pid)
	}
	sort.Ints(tids)
	return tids, nil
}

// Dumpable reports whether CRIU can dump the recorded tree as it stands, and
// names the first signal that says it cannot: CRIU released it, and the tree is
// still the one that was suspended.
func Dumpable(procRoot string, recorded TreeSnapshot) (bool, string) {
	pids := sortedPIDs(recorded)
	if released, reason := Released(procRoot, pids); !released {
		return false, reason
	}
	for _, pid := range pids {
		tids, err := taskIDs(procRoot, pid)
		if err != nil {
			return false, fmt.Sprintf("process %d is gone or unreadable: %v", pid, err)
		}
		for _, tid := range tids {
			// A task blocked inside a driver call cannot be seized reliably, which
			// only a dump needs — so this is Dumpable's concern, not Released's.
			probe, err := probeTaskOf(procRoot, pid, tid)
			if err != nil {
				return false, fmt.Sprintf("task %d of process %d is gone or unreadable: %v", tid, pid, err)
			}
			if probe.State == "D" {
				return false, fmt.Sprintf("task %d of process %d is in uninterruptible sleep", tid, pid)
			}
		}
		probe, err := ProbeTask(procRoot, pid)
		if err != nil {
			return false, fmt.Sprintf("process %d is gone or unreadable: %v", pid, err)
		}
		if probe.Threads != recorded[pid] {
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
