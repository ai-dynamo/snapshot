// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
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
		"pid list mismatch": {pids: []int{101}, wantErr: true},
		"no sockets": {
			setup: func(t *testing.T, root string) { mustControlDir(t, root, 101, 1) },
			pids:  []int{101, 102}, nsPIDs: []int{1, 2},
		},
		"procfs environ is not evidence": {
			setup: func(t *testing.T, root string) {
				mustControlDir(t, root, 101, 1)
				mustEnviron(t, root, 101, "LD_PRELOAD="+podcontract.CuinterposeLibraryPath+"\x00")
			},
			pids: []int{101}, nsPIDs: []int{1},
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

func TestPrepareCuinterposeRejectsInvalidTargetPIDBeforeOpeningInputs(t *testing.T) {
	err := PrepareCuinterpose(
		context.Background(),
		"/missing-checkpoint",
		"/missing-proc",
		0,
		[]int{1},
		"/missing-coordinator",
	)
	if err == nil || !strings.Contains(err.Error(), "invalid cuinterpose target PID") {
		t.Fatalf("PrepareCuinterpose() error = %v, want invalid target PID", err)
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
	if _, err := RemoveStaleCuinterposeSockets(control, []int{0}); err == nil {
		t.Fatal("a zero namespace PID must be rejected")
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

func fakeProcessNamespaces(t *testing.T, procRoot string, pid int) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid), "ns")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{"mnt", "uts", "ipc", "net", "pid"} {
		if err := os.Symlink(filepath.Join("/proc/self/ns", namespace), filepath.Join(dir, namespace)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/", filepath.Join(procRoot, strconv.Itoa(pid), "root")); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorArgvContract(t *testing.T) {
	binary, argvFile := fakeCoordinator(t, 0)
	nsenterArgvFile := fakeNSenter(t)
	procRoot := t.TempDir()
	fakeProcessNamespaces(t, procRoot, 4242)
	err := PrepareCuinterpose(
		context.Background(),
		t.TempDir(),
		procRoot,
		4242,
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
	args, err := cuinterposeArgs("prepare", "/c", podcontract.SnapshotControlMountPath, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), binary, args...)
	err = executeCoordinator(cmd, binary, args[0])
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"exit status 3", "prepare failed: participant prepare"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func TestCuinterposeArgsRejectsEmptyOrInvalidPIDs(t *testing.T) {
	if _, err := cuinterposeArgs("prepare", "/c", "/snapshot-control", nil); err == nil {
		t.Fatal("no processes must be an error")
	}
	if _, err := cuinterposeArgs("prepare", "/c", "/snapshot-control", []int{0}); err == nil {
		t.Fatal("zero namespace PID must be an error")
	}
}

// Keep endpoint discovery and artifact names aligned with Rust without a second
// wire-protocol implementation.
func TestGoConstantsMatchTheRustSources(t *testing.T) {
	root := filepath.Join("..", "..", "cmd", "cuinterpose", "rust")
	core, err := os.ReadFile(filepath.Join(root, "core", "src", "runtime", "mod.rs"))
	if err != nil {
		t.Fatal(err)
	}
	protocol, err := os.ReadFile(filepath.Join(root, "protocol", "src", "lib.rs"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := os.ReadFile(filepath.Join(root, "coordinator", "src", "main.rs"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(protocol), "pub const VERSION: u8 = "+strconv.Itoa(types.CuinterposeFormat)+";") {
		t.Error("Rust protocol version differs from the Go checkpoint artifact format")
	}
	if !strings.Contains(string(protocol), cuinterposeSocketPrefix) {
		t.Errorf("Rust protocol lacks endpoint contract %q", cuinterposeSocketPrefix)
	}
	for _, value := range []string{podcontract.SnapshotControlMountPath, podcontract.SnapshotControlDirEnv} {
		if !strings.Contains(string(core), value) {
			t.Errorf("Rust core lacks endpoint contract %q", value)
		}
	}
	if !strings.Contains(string(coordinator), `"`+CuinterposeStateFile+`"`) {
		t.Error("Rust coordinator state filename differs from Go")
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

func mustControlDir(t *testing.T, procRoot string, observedPID, namespacePID int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(cuinterposeEndpointPath(procRoot, observedPID, namespacePID)), 0700); err != nil {
		t.Fatal(err)
	}
}

func mustEnviron(t *testing.T, procRoot string, observedPID int, content string) {
	t.Helper()
	path := filepath.Join(procRoot, strconv.Itoa(observedPID), "environ")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
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
