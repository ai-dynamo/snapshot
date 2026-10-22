// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeProcStatus lays down one /proc/<pid>/status entry with the three fields
// the probe reads, plus neighbours so field matching has to be exact.
func writeProcStatus(t *testing.T, procRoot string, pid, tracerPID, threads int, state string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
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
