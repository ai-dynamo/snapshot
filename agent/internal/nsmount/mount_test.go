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
	m, err := newExecMounter(bin, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	return m
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

	_, err := m.Mount(context.Background(), os.Getpid(), "/src/artifact", CheckpointDst, MountOptions{ReadOnly: true, NoExec: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mount-fd", fmt.Sprintf("%d", nsFdChildNum), "/src/artifact", CheckpointDst, "ro", "noexec"}
	got := readLines(t, logFile)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

func TestExecMounterMountErrorWrapped(t *testing.T) {
	bin := writeFakeBinary(t, `echo "subprocess boom" >&2; exit 1`)
	_, err := newMounterForTest(t, bin).Mount(context.Background(), os.Getpid(), "/src/artifact", "/dst", MountOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"/src/artifact", "/dst", "subprocess boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestExecMounterMountNsFdOpenFailure(t *testing.T) {
	bin := writeFakeBinary(t, `exit 0`)
	_, err := newMounterForTest(t, bin).Mount(context.Background(), math.MaxInt32, "/src/artifact", "/dst", MountOptions{})
	if err == nil || !strings.Contains(err.Error(), "ns/mnt") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecMounterUnmountErrorAndIdempotence(t *testing.T) {
	bin := writeFakeBinary(t, `if [ "$1" = "umount-fd" ]; then echo "boom" >&2; exit 1; fi`)
	handle, err := newMounterForTest(t, bin).Mount(context.Background(), os.Getpid(), "/src/artifact", "/dst", MountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = handle.Unmount()
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected unmount error: %v", err)
	}
	if err2 := handle.Unmount(); err2 != err {
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
		name, source, destination string
	}{
		{name: "outside root", source: "/etc"},
		{name: "proc", source: "/proc"},
		{name: "sibling prefix", source: "/checkpoints-other/id"},
		{name: "traversal", source: "/checkpoints/../etc"},
		{name: "repeated separator", source: "/checkpoints//id"},
		{name: "whitespace", source: "/checkpoints/bad id"},
		{name: "shell punctuation", source: "/checkpoints/bad;id"},
		{name: "unicode", source: "/checkpoints/é"},
		{name: "checkpoint source for bundle role", source: "/checkpoints/id", destination: SnapshotBinDst},
		{name: "bundle source for checkpoint role", source: SnapshotBinSrc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := tc.destination
			if destination == "" {
				destination = CheckpointDst
			}
			cmd := exec.Command(binary, "mount-fd", fmt.Sprintf("%d", nsFdChildNum), tc.source, destination, "ro", "noexec")
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("helper accepted source %q", tc.source)
			}
			if strings.Contains(string(output), "open_tree") || strings.Contains(string(output), "setns") {
				t.Fatalf("helper reached mount syscall for invalid source: %s", output)
			}
		})
	}
}
