// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nsmount

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

func writeFakeBinary(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ns-bind-mount")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newMounterForTest(t *testing.T, bin string) *execMounter {
	t.Helper()
	return newExecMounter(bin, logr.Discard())
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestExecMounterMountArgs(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "args.log")
	bin := writeFakeBinary(t, `printf '%s\n' "$@" >> `+logFile)
	m := newMounterForTest(t, bin)
	nsFd, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	defer nsFd.Close()

	handle, err := m.MountCheckpoint(context.Background(), nsFd, "/checkpoints/abc/versions/1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := handle.Unmount(context.Background()); err != nil {
			t.Errorf("Unmount: %v", err)
		}
	})
	want := []string{"mount-checkpoint-fd", fmt.Sprintf("%d", nsFdChildNum), "/checkpoints/abc/versions/1"}
	got := readLines(t, logFile)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestExecMounterMountErrorWrapped(t *testing.T) {
	bin := writeFakeBinary(t, `echo "subprocess boom" >&2; exit 1`)
	nsFd, openErr := os.Open("/proc/self/ns/mnt")
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer nsFd.Close()
	_, err := newMounterForTest(t, bin).MountCheckpoint(context.Background(), nsFd, "/checkpoints/abc/versions/1")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"mount-checkpoint-fd", "subprocess boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestExecMounterCheckpointUsesPinnedBundleNamespace(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "namespaces.log")
	bin := writeFakeBinary(t, `printf '%s %s\n' "$1" "$(readlink /proc/self/fd/$2)" >> `+logFile)
	m := newMounterForTest(t, bin)

	bundle, err := m.MountBundle(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := bundle.Unmount(context.Background()); err != nil {
			t.Errorf("unmount bundle: %v", err)
		}
	})
	checkpoint, err := m.MountCheckpoint(context.Background(), bundle.NsFd(), "/checkpoints/abc/versions/1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkpoint.Unmount(context.Background()); err != nil {
			t.Errorf("unmount checkpoint: %v", err)
		}
	})

	lines := readLines(t, logFile)
	if len(lines) != 2 {
		t.Fatalf("mount helper calls = %v, want bundle and checkpoint", lines)
	}
	bundleFields := strings.Fields(lines[0])
	checkpointFields := strings.Fields(lines[1])
	if len(bundleFields) != 2 || len(checkpointFields) != 2 {
		t.Fatalf("unexpected mount helper output: %v", lines)
	}
	if bundleFields[1] != checkpointFields[1] {
		t.Fatalf("namespace fds resolve to %q and %q, want the same namespace", bundleFields[1], checkpointFields[1])
	}
}

func TestExecMounterMountNsFdOpenFailure(t *testing.T) {
	bin := writeFakeBinary(t, `exit 0`)
	_, err := newMounterForTest(t, bin).MountBundle(context.Background(), math.MaxInt32)
	if err == nil || !strings.Contains(err.Error(), "ns/mnt") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecMounterUnmountErrorAndIdempotence(t *testing.T) {
	bin := writeFakeBinary(t, `if [ "$1" = "unmount-bundle-fd" ]; then echo "boom" >&2; exit 1; fi`)
	handle, err := newMounterForTest(t, bin).MountBundle(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	err = handle.Unmount(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected unmount error: %v", err)
	}
	if err2 := handle.Unmount(context.Background()); err2 != err {
		t.Fatalf("second Unmount() = %v, want same error %v", err2, err)
	}
}

func TestCHelperRejectsUnsafeSourcesBeforeMountSyscalls(t *testing.T) {
	gcc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skip("gcc is required to validate the C helper")
	}
	binary := filepath.Join(t.TempDir(), "ns-bind-mount")
	source := filepath.Join("..", "..", "cmd", "ns-bind-mount", "main.c")
	compile := exec.Command(gcc, "-O2", "-Wall", "-Wextra", "-o", binary, source)
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile C helper: %v\n%s", err, output)
	}

	for _, tc := range []struct {
		name, source string
	}{
		{name: "outside root", source: "/etc"},
		{name: "proc", source: "/proc"},
		{name: "sibling prefix", source: "/checkpoints-other/id"},
		{name: "traversal", source: "/checkpoints/../etc"},
		{name: "repeated separator", source: "/checkpoints//id"},
		{name: "whitespace", source: "/checkpoints/bad id"},
		{name: "shell punctuation", source: "/checkpoints/bad;id"},
		{name: "unicode", source: "/checkpoints/é"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(binary, "mount-checkpoint-fd", fmt.Sprintf("%d", nsFdChildNum), tc.source)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("helper accepted source %q", tc.source)
			}
			if strings.Contains(string(output), "open_tree") || strings.Contains(string(output), "setns") {
				t.Fatalf("helper reached mount syscall for invalid source: %s", output)
			}
		})
	}

	for _, args := range [][]string{
		{"mount-fd", "3", "/etc", "/tmp/checkpoint"},
		{"mount-bundle-fd", "3", "/etc"},
		{"unmount-checkpoint-fd", "3", "unexpected"},
	} {
		if output, err := exec.Command(binary, args...).CombinedOutput(); err == nil {
			t.Fatalf("helper accepted %v: %s", args, output)
		}
	}
}
