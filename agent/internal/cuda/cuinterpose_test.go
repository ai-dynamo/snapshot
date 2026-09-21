// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestDetectCuinterpose(t *testing.T) {
	cases := map[string]struct {
		setup        func(t *testing.T, procRoot string)
		pids, nsPIDs []int
		want         bool
		wantErr      bool
	}{
		"no CUDA processes": {},
		"no sockets": {
			pids: []int{101, 102}, nsPIDs: []int{1, 2},
		},
		"one of two sockets missing": {
			setup: func(t *testing.T, root string) { listenUnix(t, cuinterposeEndpointPath(root, 101, 1)) },
			pids:  []int{101, 102}, nsPIDs: []int{1, 2}, wantErr: true,
		},
		"every process has a socket": {
			setup: func(t *testing.T, root string) {
				listenUnix(t, cuinterposeEndpointPath(root, 101, 1))
				listenUnix(t, cuinterposeEndpointPath(root, 102, 2))
			},
			pids: []int{101, 102}, nsPIDs: []int{1, 2}, want: true,
		},
		"endpoint is not a socket": {
			setup: func(t *testing.T, root string) {
				listenUnix(t, cuinterposeEndpointPath(root, 101, 1))
				path := cuinterposeEndpointPath(root, 102, 2)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("not a socket"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			pids: []int{101, 102}, nsPIDs: []int{1, 2}, wantErr: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			procRoot := shortTempDir(t)
			if tc.setup != nil {
				tc.setup(t, procRoot)
			}
			got, err := DetectCuinterpose(procRoot, tc.pids, tc.nsPIDs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("DetectCuinterpose() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("DetectCuinterpose() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRemoveStaleCuinterposeSockets(t *testing.T) {
	control := shortTempDir(t)
	listenUnix(t, filepath.Join(control, cuinterposeSocketName(7)))
	listenUnix(t, filepath.Join(control, cuinterposeSocketName(8)))
	listenUnix(t, filepath.Join(control, cuinterposeSocketName(9)))
	for _, keep := range []string{"restore-complete", "workload-ready", "other-1.sock"} {
		if err := os.WriteFile(filepath.Join(control, keep), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := RemoveStaleCuinterposeSockets(control, []int{7, 8})
	if err != nil {
		t.Fatalf("RemoveStaleCuinterposeSockets() error = %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed %d, want 2", removed)
	}
	entries, _ := os.ReadDir(control)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "cuinterpose-9.sock,other-1.sock,restore-complete,workload-ready" {
		t.Fatalf("unexpected leftovers: %v", names)
	}
}

// A shell script standing in for the coordinator: records its argv, prints
// an error diagnostic, and exits as told.
func fakeCoordinator(t *testing.T, exitCode int) (binary, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	binary = filepath.Join(dir, "coordinator.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"echo 'prepare failed: participant prepare' >&2\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return binary, argvFile
}

func fakeNSenter(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "nsenter-argv")
	binary := filepath.Join(dir, "nsenter")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"while [ \"$1\" != -- ]; do shift; done\n" +
		"shift\n" +
		"exec \"$@\"\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return argvFile
}

func TestCoordinatorArgvContract(t *testing.T) {
	binary, argvFile := fakeCoordinator(t, 0)
	nsenterArgvFile := fakeNSenter(t)
	err := PrepareCuinterpose(
		context.Background(),
		t.TempDir(),
		"/proc",
		os.Getpid(),
		[]int{7, 9},
		binary,
	)
	if err != nil {
		t.Fatalf("PrepareCuinterpose() error = %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	want := strings.Join([]string{
		"--prepare", "--checkpoint-dir", "/proc/self/fd/4",
		"--control-dir", podcontract.SnapshotControlMountPath,
		"--process", "7", "--process", "9", "",
	}, "\n")
	if string(argv) != want {
		t.Fatalf("argv:\n%s\nwant:\n%s", argv, want)
	}
	nsenterArgv, _ := os.ReadFile(nsenterArgvFile)
	wantNSenterPrefix := strings.Join([]string{
		"--mount=/proc/self/fd/5",
		"--uts=/proc/self/fd/6",
		"--ipc=/proc/self/fd/7",
		"--net=/proc/self/fd/8",
		"--pid=/proc/self/fd/9",
		"--root=/proc/self/fd/10",
		"--wd=/proc/self/fd/10",
		"--",
		"/proc/self/fd/3",
		"",
	}, "\n")
	if !strings.HasPrefix(string(nsenterArgv), wantNSenterPrefix) {
		t.Fatalf("nsenter argv:\n%s\nwant prefix:\n%s", nsenterArgv, wantNSenterPrefix)
	}

	// Restore already runs inside the restored namespaces.
	binary, argvFile = fakeCoordinator(t, 0)
	if err := RestoreCuinterpose(context.Background(), "/tmp/checkpoint", []int{1}, binary); err != nil {
		t.Fatalf("RestoreCuinterpose() error = %v", err)
	}
	argv, _ = os.ReadFile(argvFile)
	if !strings.HasPrefix(string(argv), "--restore\n--checkpoint-dir\n/tmp/checkpoint\n--control-dir\n"+podcontract.SnapshotControlMountPath+"\n") {
		t.Fatalf("restore argv:\n%s", argv)
	}
}

func TestCoordinatorFailureIncludesStderr(t *testing.T) {
	binary, _ := fakeCoordinator(t, 3)
	err := RestoreCuinterpose(context.Background(), "/checkpoint", []int{1}, binary)
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"exit status 3", "prepare failed: participant prepare"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to 108 bytes; t.TempDir() paths are long.
	dir, err := os.MkdirTemp("", "cui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func listenUnix(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
}

func TestCheckCuinterposeEnablement(t *testing.T) {
	cases := []struct {
		requested, detected bool
		cuda                int
		wantErr             bool
	}{
		{false, false, 0, false},
		{true, false, 0, false}, // no CUDA processes: nothing to interpose
		{false, false, 4, false},
		{true, true, 4, false},
		{true, false, 4, true}, // asked for, shim never loaded
		{false, true, 4, true}, // shim present without the opt-in
	}
	for _, tc := range cases {
		err := CheckCuinterposeEnablement(tc.requested, tc.detected, tc.cuda)
		if (err != nil) != tc.wantErr {
			t.Errorf("CheckCuinterposeEnablement(%v, %v, %d) = %v, wantErr %v", tc.requested, tc.detected, tc.cuda, err, tc.wantErr)
		}
	}
}
