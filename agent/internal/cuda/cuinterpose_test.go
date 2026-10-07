// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeCoordinator records its argv and, for prepare, the directory its inherited
// checkpoint descriptor refers to. It reports an error only when it fails.
func fakeCoordinator(t *testing.T, exitCode int) (binary, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	binary = filepath.Join(dir, "coordinator.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"if [ \"$1\" = --prepare ]; then readlink -f \"$3\" > " + argvFile + ".checkpoint; fi\n" +
		"if [ " + strconv.Itoa(exitCode) + " -ne 0 ]; then echo 'coordinator failed: participant refused' >&2; fi\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return binary, argvFile
}

func fakeNSenter(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "nsenter")
	script := "#!/bin/sh\n" +
		"while [ \"$1\" != -- ]; do shift; done\n" +
		"shift\n" +
		"exec \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestCoordinatorArgvContract(t *testing.T) {
	binary, argvFile := fakeCoordinator(t, 0)
	fakeNSenter(t)
	checkpointDir := t.TempDir()
	target := CuInterposeTarget{ProcRoot: "/proc", HostPID: os.Getpid(), NamespacePIDs: []int{7, 9}, Binary: binary}
	err := PrepareCuInterpose(context.Background(), target, checkpointDir)
	if err != nil {
		t.Fatalf("PrepareCuInterpose() error = %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	// The coordinator runs inside the container, so it receives the checkpoint
	// directory as an inherited descriptor rather than a host path.
	fields := strings.Split(string(argv), "\n")
	require.Regexp(t, `^/proc/self/fd/[0-9]+$`, fields[2])
	fields[2] = "<checkpoint fd>"
	require.Equal(t, []string{
		"--prepare", "--checkpoint-dir", "<checkpoint fd>",
		"--socket-dir", cuInterposeSocketDir,
		"--process", "7", "--process", "9", "",
	}, fields)
	resolved, _ := os.ReadFile(argvFile + ".checkpoint")
	wantDir, err := filepath.EvalSymlinks(checkpointDir)
	require.NoError(t, err)
	require.Equal(t, wantDir, strings.TrimSpace(string(resolved)))
	if err := InspectCuInterpose(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	argv, _ = os.ReadFile(argvFile)
	want := strings.Join([]string{
		"--inspect", "--socket-dir", cuInterposeSocketDir,
		"--process", "7", "--process", "9", "",
	}, "\n")
	if string(argv) != want {
		t.Fatalf("inspect argv:\n%s\nwant:\n%s", argv, want)
	}

	// Restore already runs inside the restored namespaces.
	binary, argvFile = fakeCoordinator(t, 0)
	if err := RestoreCuInterpose(context.Background(), "/tmp/checkpoint", []int{623}, binary); err != nil {
		t.Fatalf("RestoreCuInterpose() error = %v", err)
	}
	argv, _ = os.ReadFile(argvFile)
	want = strings.Join([]string{
		"--restore", "--checkpoint-dir", "/tmp/checkpoint",
		"--socket-dir", cuInterposeSocketDir, "--process", "623", "",
	}, "\n")
	if string(argv) != want {
		t.Fatalf("restore argv:\n%s", argv)
	}
}

func TestCoordinatorFailureIncludesStderr(t *testing.T) {
	binary, _ := fakeCoordinator(t, 3)
	fakeNSenter(t)
	target := CuInterposeTarget{ProcRoot: "/proc", HostPID: os.Getpid(), NamespacePIDs: []int{1}, Binary: binary}
	for _, err := range []error{
		InspectCuInterpose(context.Background(), target),
		PrepareCuInterpose(context.Background(), target, t.TempDir()),
		RestoreCuInterpose(context.Background(), "/checkpoint", []int{1}, binary),
	} {
		if err == nil {
			t.Fatal("expected failure")
		}
		for _, want := range []string{"exit status 3", "coordinator failed: participant refused"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q lacks %q", err, want)
			}
		}
	}
}
