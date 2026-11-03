// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeProcStatus lays down one /proc/<pid>/status entry with the three fields
// the probe reads, plus neighbours so field matching has to be exact.
// writeProcStatus lays down a process and its thread-group leader task, as a
// real procfs has both.
func writeProcStatus(t *testing.T, procRoot string, pid, tracerPID, threads int, state string) {
	t.Helper()
	writeStatusFile(t, filepath.Join(procRoot, strconv.Itoa(pid)), pid, tracerPID, threads, state)
	writeTaskStatus(t, procRoot, pid, pid, tracerPID, threads, state)
}

// writeTaskStatus lays down one task of a process.
func writeTaskStatus(t *testing.T, procRoot string, pid, tid, tracerPID, threads int, state string) {
	t.Helper()
	writeStatusFile(t, filepath.Join(procRoot, strconv.Itoa(pid), "task", strconv.Itoa(tid)),
		tid, tracerPID, threads, state)
}

func writeStatusFile(t *testing.T, dir string, pid, tracerPID, threads int, state string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Name:\tpython\nState:\t%s (sleeping)\nPPid:\t1\nTracerPid:\t%d\nThreads:\t%d\n",
		state, tracerPID, threads)
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProbeTaskReadsTheSeizureSignals(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatus(t, procRoot, 42, 7, 12, "S")

	probe, err := ProbeTask(procRoot, 42)
	if err != nil {
		t.Fatalf("ProbeTask() = %v", err)
	}
	if probe.TracerPID != 7 || probe.Threads != 12 || probe.State != "S" {
		t.Errorf("ProbeTask() = %+v", probe)
	}
}

func TestProbeTaskRejectsIncompleteStatus(t *testing.T) {
	procRoot := t.TempDir()
	dir := filepath.Join(procRoot, "42")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\tpython\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ProbeTask(procRoot, 42); err == nil {
		t.Error("status missing every probed field should not parse")
	}
}

// Each signal has to fail the tree on its own, or a retry dumps a process CRIU
// cannot seize.
func TestDumpable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tracerPID int
		state     string
		threads   int
		gone      bool
		want      bool
	}{
		{name: "released and unchanged", state: "S", threads: 4, want: true},
		{name: "running counts as released", state: "R", threads: 4, want: true},
		{name: "still traced", tracerPID: 99, state: "S", threads: 4},
		{name: "stopped", state: "T", threads: 4},
		{name: "traced-and-stopped", state: "t", threads: 4},
		{name: "uninterruptible sleep", state: "D", threads: 4},
		{name: "a thread exited", state: "S", threads: 3},
		{name: "a thread was created", state: "S", threads: 5},
		{name: "the process is gone", gone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procRoot := t.TempDir()
			recorded := TreeSnapshot{42: 4}
			if !tc.gone {
				writeProcStatus(t, procRoot, 42, tc.tracerPID, tc.threads, tc.state)
			}

			got, reason := Dumpable(procRoot, recorded)

			if got != tc.want {
				t.Errorf("Dumpable() = %v (%s), want %v", got, reason, tc.want)
			}
			if !got && reason == "" {
				t.Error("a tree that cannot be dumped must say why")
			}
		})
	}
}

func TestSnapshotTreeRecordsEveryProcess(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatus(t, procRoot, 42, 0, 4, "S")
	writeProcStatus(t, procRoot, 43, 0, 1, "S")

	snapshot, err := SnapshotTree(procRoot, []int{42, 43})
	if err != nil {
		t.Fatalf("SnapshotTree() = %v", err)
	}
	if snapshot[42] != 4 || snapshot[43] != 1 {
		t.Errorf("SnapshotTree() = %v", snapshot)
	}

	if _, err := SnapshotTree(procRoot, []int{42, 44}); err == nil {
		t.Error("a process that cannot be read should not snapshot silently")
	}
}

func TestGPUStateUniform(t *testing.T) {
	if ok, _ := GPUStateUniform(map[int]string{42: "checkpointed", 43: "checkpointed"}, "checkpointed"); !ok {
		t.Error("a uniformly checkpointed tree should pass")
	}
	ok, reason := GPUStateUniform(map[int]string{42: "checkpointed", 43: "locked"}, "checkpointed")
	if ok {
		t.Error("a half-suspended tree is not the one the dump was meant to capture")
	}
	if reason == "" {
		t.Error("a mixed tree must say which process differs")
	}
	if ok, _ := GPUStateUniform(nil, "checkpointed"); !ok {
		t.Error("a tree with no GPU processes is vacuously uniform")
	}
}

// Released is the half of the check that stands on its own: a task CRIU never
// let go of cannot resume, whatever the tree looked like at suspend.
func TestReleased(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tracerPID int
		state     string
		want      bool
	}{
		{name: "released", state: "S", want: true},
		{name: "running", state: "R", want: true},
		{name: "still traced", tracerPID: 99, state: "S"},
		{name: "stopped", state: "T"},
		{name: "traced-and-stopped", state: "t"},
		{name: "uninterruptible sleep still resumes on its own", state: "D", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procRoot := t.TempDir()
			writeProcStatus(t, procRoot, 42, tc.tracerPID, 4, tc.state)

			got, reason := Released(procRoot, []int{42})

			if got != tc.want {
				t.Errorf("Released() = %v (%s), want %v", got, reason, tc.want)
			}
			if !got && reason == "" {
				t.Error("a task that was not released must say why")
			}
		})
	}
}

// Thread count is Dumpable's concern alone: a tree that forked after CRIU let
// go is still released, just no longer the tree that was suspended.
func TestReleasedIgnoresThreadCount(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatus(t, procRoot, 42, 0, 9, "S")

	if released, reason := Released(procRoot, []int{42}); !released {
		t.Errorf("Released() = false (%s), want true", reason)
	}
	if dumpable, _ := Dumpable(procRoot, TreeSnapshot{42: 4}); dumpable {
		t.Error("Dumpable() = true, want false for a changed tree")
	}
}

// CRIU seizes tasks, not processes. A non-leader thread left traced or stopped
// keeps the tree from resuming even when its leader reads perfectly clean, so
// checking only the leader would call that tree released and leave it hung.
func TestReleasedChecksEveryTask(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tracerPID int
		state     string
	}{
		{name: "a non-leader task is still traced", tracerPID: 99, state: "S"},
		{name: "a non-leader task is stopped", state: "T"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procRoot := t.TempDir()
			writeProcStatus(t, procRoot, 42, 0, 2, "S")
			writeTaskStatus(t, procRoot, 42, 43, tc.tracerPID, 2, tc.state)

			released, reason := Released(procRoot, []int{42})

			if released {
				t.Error("a tree with a seized thread was reported released")
			}
			if !strings.Contains(reason, "43") {
				t.Errorf("reason = %q, want it to name task 43", reason)
			}
		})
	}
}

func TestReleasedAcceptsATreeWhoseTasksAreAllClean(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatus(t, procRoot, 42, 0, 2, "S")
	writeTaskStatus(t, procRoot, 42, 43, 0, 2, "R")

	if released, reason := Released(procRoot, []int{42}); !released {
		t.Errorf("Released() = false (%s), want true", reason)
	}
}

// Uninterruptible sleep ends by itself, and a thread in a driver call is
// ordinary right after a GPU restore — so it must not fail the post-revive
// check, only the dump.
func TestUninterruptibleSleepFailsOnlyTheDump(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatus(t, procRoot, 42, 0, 1, "D")

	if released, reason := Released(procRoot, []int{42}); !released {
		t.Errorf("Released() = false (%s), want true: a D task can still resume", reason)
	}
	if dumpable, _ := Dumpable(procRoot, TreeSnapshot{42: 1}); dumpable {
		t.Error("Dumpable() = true, want false: CRIU cannot seize a D task")
	}
}
