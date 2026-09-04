// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestDetectCuinterposeSkipsEmptyPIDList(t *testing.T) {
	interposed, err := DetectCuinterpose(t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("DetectCuinterpose() error = %v", err)
	}
	if interposed {
		t.Fatal("DetectCuinterpose() = true, want skip")
	}
}

func TestDetectCuinterposePIDCountMismatch(t *testing.T) {
	if _, err := DetectCuinterpose(t.TempDir(), []int{101}, nil); err == nil {
		t.Fatal("expected PID mapping count mismatch")
	}
}

func TestDetectCuinterposeSkipsWithoutSockets(t *testing.T) {
	procRoot := shortTempDir(t)
	mustControlDir(t, procRoot, 101, 1)
	interposed, err := DetectCuinterpose(procRoot, []int{101, 102}, []int{1, 2})
	if err != nil {
		t.Fatalf("DetectCuinterpose() error = %v", err)
	}
	if interposed {
		t.Fatal("DetectCuinterpose() = true, want skip")
	}
}

func TestDetectCuinterposeIgnoresProcfsEnviron(t *testing.T) {
	procRoot := shortTempDir(t)
	mustControlDir(t, procRoot, 101, 1)
	mustEnviron(t, procRoot, 101, "LD_PRELOAD="+podcontract.CuinterposeLibraryPath+"\x00")
	interposed, err := DetectCuinterpose(procRoot, []int{101}, []int{1})
	if err != nil {
		t.Fatalf("DetectCuinterpose() error = %v", err)
	}
	if interposed {
		t.Fatal("DetectCuinterpose() keyed off procfs environ; only sockets count")
	}
}

func TestDetectCuinterposeRequiresEveryCUDAProcessSocket(t *testing.T) {
	procRoot := shortTempDir(t)
	listenUnix(t, cuinterposeEndpointPath(procRoot, 101, 1))

	if _, err := DetectCuinterpose(procRoot, []int{101, 102}, []int{1, 2}); err == nil {
		t.Fatal("expected a missing endpoint to fail closed")
	}

	listenUnix(t, cuinterposeEndpointPath(procRoot, 102, 2))
	interposed, err := DetectCuinterpose(procRoot, []int{101, 102}, []int{1, 2})
	if err != nil {
		t.Fatalf("DetectCuinterpose() error = %v", err)
	}
	if !interposed {
		t.Fatal("DetectCuinterpose() = false, want true")
	}
}

func TestDetectCuinterposeRejectsNonSocketEndpoint(t *testing.T) {
	procRoot := shortTempDir(t)
	listenUnix(t, cuinterposeEndpointPath(procRoot, 101, 1))
	path := cuinterposeEndpointPath(procRoot, 102, 2)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := DetectCuinterpose(procRoot, []int{101, 102}, []int{1, 2}); err == nil {
		t.Fatal("expected a non-socket endpoint to fail closed")
	}
}

func TestHasCuinterposeState(t *testing.T) {
	checkpointDir := t.TempDir()
	present, err := HasCuinterposeState(checkpointDir)
	if err != nil || present {
		t.Fatalf("HasCuinterposeState() = %v, %v for a missing file", present, err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, CuinterposeStateFile), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	present, err = HasCuinterposeState(checkpointDir)
	if err != nil || !present {
		t.Fatalf("HasCuinterposeState() = %v, %v, want true", present, err)
	}
}

func TestRemoveStaleCuinterposeSockets(t *testing.T) {
	control := shortTempDir(t)
	listenUnix(t, filepath.Join(control, cuinterposeSocketName(7)))
	listenUnix(t, filepath.Join(control, cuinterposeSocketName(8)))
	for _, keep := range []string{"restore-complete", "cuda-checkpoint-job", "other-1.sock"} {
		if err := os.WriteFile(filepath.Join(control, keep), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := RemoveStaleCuinterposeSockets(control)
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
	if strings.Join(names, ",") != "cuda-checkpoint-job,other-1.sock,restore-complete" {
		t.Fatalf("unexpected leftovers: %v", names)
	}
	if _, err := RemoveStaleCuinterposeSockets(filepath.Join(control, "missing")); err == nil {
		t.Fatal("a missing control directory must be an error, not a silent no-op")
	}
}

func TestParseCoordinatorReport(t *testing.T) {
	phase, ok := parseCoordinatorReport("cuinterpose-coordinator phase=save_host_carrier status=ok elapsed_ms=12.5 participants=8 carrier_count=3 carrier_bytes=1610612736 gb_per_s=41.20")
	if !ok || phase.Phase != "save_host_carrier" {
		t.Fatalf("parse = %+v, %v", phase, ok)
	}
	if phase.Fields["carrier_bytes"] != "1610612736" || phase.Fields["gb_per_s"] != "41.20" || phase.Fields["status"] != "ok" {
		t.Fatalf("fields = %v", phase.Fields)
	}
	for _, junk := range []string{"", "prepare failed: participant inspect", "cuinterpose-coordinator status=ok", "phase=inspect"} {
		if _, ok := parseCoordinatorReport(junk); ok {
			t.Fatalf("parsed %q as a report", junk)
		}
	}
}

// A shell script standing in for the coordinator: records its argv, prints
// two progress lines, and exits as told.
func fakeCoordinator(t *testing.T, exitCode int) (binary, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	binary = filepath.Join(dir, "coordinator.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"echo 'cuinterpose-coordinator phase=inspect status=ok elapsed_ms=1.0 participants=2 records=4'\n" +
		"echo 'not a report line'\n" +
		"echo 'cuinterpose-coordinator phase=validate status=ok elapsed_ms=0.2 participants=2'\n" +
		"echo 'prepare failed: participant prepare' >&2\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return binary, argvFile
}

func TestPrepareCuinterposeArgvContractAndReports(t *testing.T) {
	binary, argvFile := fakeCoordinator(t, 0)
	phases, err := PrepareCuinterpose(context.Background(), "/checkpoints/x", "/host/proc", []int{4242, 4243}, []int{7, 9}, binary, testr.New(t))
	if err != nil {
		t.Fatalf("PrepareCuinterpose() error = %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	want := strings.Join([]string{
		"--prepare", "--proc-root", "/host/proc", "--checkpoint-dir", "/checkpoints/x",
		"--control-dir", podcontract.SnapshotControlMountPath,
		"--process", "4242", "7", "--process", "4243", "9", "",
	}, "\n")
	if string(argv) != want {
		t.Fatalf("argv:\n%s\nwant:\n%s", argv, want)
	}
	if len(phases) != 2 || phases[0].Phase != "inspect" || phases[1].Phase != "validate" || phases[0].Fields["records"] != "4" {
		t.Fatalf("phases = %+v", phases)
	}
}

func TestRestoreCuinterposeArgvUsesEmptyProcRoot(t *testing.T) {
	binary, argvFile := fakeCoordinator(t, 0)
	if _, err := RestoreCuinterpose(context.Background(), "/tmp/checkpoint", []int{100}, []int{1}, binary, logr.Discard()); err != nil {
		t.Fatalf("RestoreCuinterpose() error = %v", err)
	}
	argv, _ := os.ReadFile(argvFile)
	if !strings.HasPrefix(string(argv), "--restore\n--proc-root\n\n--checkpoint-dir\n/tmp/checkpoint\n--control-dir\n"+podcontract.SnapshotControlMountPath+"\n") {
		t.Fatalf("argv:\n%s", argv)
	}
}

func TestCoordinatorFailureReportsStderrAndCompletedPhases(t *testing.T) {
	binary, _ := fakeCoordinator(t, 3)
	phases, err := PrepareCuinterpose(context.Background(), "/c", "/p", []int{1}, []int{1}, binary, logr.Discard())
	if err == nil {
		t.Fatal("expected failure")
	}
	if len(phases) != 2 {
		t.Fatalf("phases before failure = %d, want 2", len(phases))
	}
	for _, want := range []string{"exit status 3", "completed phases: inspect,validate", "prepare failed: participant prepare"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func TestCuinterposeArgsRejectsEmptyOrMismatchedPIDs(t *testing.T) {
	if _, err := cuinterposeArgs("prepare", "/c", "/p", "/snapshot-control", nil, nil); err == nil {
		t.Fatal("no processes must be an error")
	}
	if _, err := cuinterposeArgs("prepare", "/c", "/p", "/snapshot-control", []int{1, 2}, []int{1}); err == nil {
		t.Fatal("mismatched PID lists must be an error")
	}
}

// The Go constants and the coordinator's command line mirror the C sources.
// This test reads them so a rename on one side cannot silently turn
// interposition off.
func TestGoConstantsMatchTheCSources(t *testing.T) {
	protocol, err := os.ReadFile(filepath.Join("..", "..", "cmd", "cuinterpose", "protocol.h"))
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := os.ReadFile(filepath.Join("..", "..", "cmd", "cuinterpose", "coordinator.c"))
	if err != nil {
		t.Fatal(err)
	}
	define := func(name string) string {
		match := regexp.MustCompile(`(?m)^#define ` + name + ` "([^"]*)"`).FindSubmatch(protocol)
		if match == nil {
			t.Fatalf("protocol.h lacks #define %s", name)
		}
		return string(match[1])
	}
	if got := define("CUINTERPOSE_SOCKET_PREFIX"); got != cuinterposeSocketPrefix {
		t.Errorf("socket prefix: C %q, Go %q", got, cuinterposeSocketPrefix)
	}
	if got := define("CUINTERPOSE_STATE_FILENAME"); got != CuinterposeStateFile {
		t.Errorf("state file: C %q, Go %q", got, CuinterposeStateFile)
	}
	if got := define("CUINTERPOSE_CONTROL_DIR"); got != podcontract.SnapshotControlMountPath {
		t.Errorf("control dir: C %q, podcontract %q", got, podcontract.SnapshotControlMountPath)
	}
	if got := define("CUINTERPOSE_CONTROL_DIR_ENV"); got != podcontract.SnapshotControlDirEnv {
		t.Errorf("control dir env: C %q, podcontract %q", got, podcontract.SnapshotControlDirEnv)
	}
	for _, flag := range []string{"--prepare", "--restore", "--proc-root", "--checkpoint-dir", "--control-dir", "--process"} {
		if !strings.Contains(string(coordinator), `"`+flag+`"`) {
			t.Errorf("coordinator.c does not parse %s", flag)
		}
	}
	if !strings.Contains(string(coordinator), `"cuinterpose-coordinator phase=%s status=ok`) {
		t.Error("coordinator.c progress line format changed; update parseCoordinatorReport")
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
