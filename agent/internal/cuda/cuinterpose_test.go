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

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestRemoveStaleCuInterposeSockets(t *testing.T) {
	control := t.TempDir()
	for _, name := range []string{"cuinterpose-7.sock", "cuinterpose-9.sock", "restore-complete", "workload-ready"} {
		if err := os.WriteFile(filepath.Join(control, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveStaleCuInterposeSockets(control, []int{7, 8}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(control)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "cuinterpose-9.sock,restore-complete,workload-ready" {
		t.Fatalf("unexpected leftovers: %v", names)
	}
}

func fakeCoordinator(t *testing.T, exitCode int) (binary, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	binary = filepath.Join(dir, "coordinator.sh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --prepare ]; then dir=$(readlink -f \"$3\"); shift 3; set -- --prepare --checkpoint-dir \"$dir\" \"$@\"; fi\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"echo 'prepare failed: participant prepare' >&2\n" +
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
	err := PrepareCuInterpose(
		context.Background(),
		checkpointDir,
		"/proc",
		os.Getpid(),
		[]int{7, 9},
		binary,
	)
	if err != nil {
		t.Fatalf("PrepareCuInterpose() error = %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	want := strings.Join([]string{
		"--prepare", "--checkpoint-dir", checkpointDir,
		"--control-dir", podcontract.SnapshotControlMountPath,
		"--process", "7", "--process", "9", "",
	}, "\n")
	if string(argv) != want {
		t.Fatalf("argv:\n%s\nwant:\n%s", argv, want)
	}

	// Restore already runs inside the restored namespaces.
	binary, argvFile = fakeCoordinator(t, 0)
	if err := RestoreCuInterpose(context.Background(), "/tmp/checkpoint", []int{1}, binary); err != nil {
		t.Fatalf("RestoreCuInterpose() error = %v", err)
	}
	argv, _ = os.ReadFile(argvFile)
	if !strings.HasPrefix(string(argv), "--restore\n--checkpoint-dir\n/tmp/checkpoint\n--control-dir\n"+podcontract.SnapshotControlMountPath+"\n") {
		t.Fatalf("restore argv:\n%s", argv)
	}
}

func TestCoordinatorFailureIncludesStderr(t *testing.T) {
	binary, _ := fakeCoordinator(t, 3)
	fakeNSenter(t)
	for _, err := range []error{
		PrepareCuInterpose(context.Background(), t.TempDir(), "/proc", os.Getpid(), []int{1}, binary),
		RestoreCuInterpose(context.Background(), "/checkpoint", []int{1}, binary),
	} {
		if err == nil {
			t.Fatal("expected failure")
		}
		for _, want := range []string{"exit status 3", "prepare failed: participant prepare"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q lacks %q", err, want)
			}
		}
	}
}
