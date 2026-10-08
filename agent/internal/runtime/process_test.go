// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestResolveHostPIDsAfterManifestMapping(t *testing.T) {
	root := t.TempDir()
	localProc, hostProc := filepath.Join(root, "local"), filepath.Join(root, "host")
	namespace := filepath.Join(root, "workload-namespace")
	otherNamespace := filepath.Join(root, "other-namespace")
	for _, path := range []string{namespace, otherNamespace} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}

	// CRIU restores a workload namespace below the placeholder namespace.
	// The host view includes one more PID level than nsrestore's proc view.
	writeProcess(t, localProc, "74", "0", "74 1", namespace)
	writeProcess(t, localProc, "80", "74", "80 750", namespace)
	writeProcess(t, hostProc, "901", "0", "901 74 1", namespace)
	writeProcess(t, hostProc, "902", "901", "902 80 750", namespace)
	writeProcess(t, hostProc, "903", "0", "903 80 750", otherNamespace)
	processes, err := ReadProcessTable(localProc)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := ResolveManifestPIDsToObservedPIDs(processes, 74, []int{750, 1})
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(hostProc)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := os.Rename(hostProc, hostProc+"-moved"); err != nil {
		t.Fatal(err)
	}
	retained := "/proc/self/fd/" + strconv.Itoa(int(directory.Fd()))
	got, err := ResolveHostPIDs(retained, localProc, observed)
	if err != nil || len(got) != 2 || got[0] != 902 || got[1] != 901 {
		t.Fatalf("host PIDs = %v, %v, want [902 901]", got, err)
	}
	for _, pids := range [][]int{{999}, {80, 80}, {0}} {
		if _, err := ResolveHostPIDs(retained, localProc, pids); err == nil {
			t.Fatalf("accepted invalid targets %v", pids)
		}
	}
	writeProcess(t, retained, "904", "0", "904 80 750", namespace)
	if _, err := ResolveHostPIDs(retained, localProc, observed); err == nil {
		t.Fatal("accepted ambiguous host PID")
	}
	for _, pid := range []string{"902", "904"} {
		if err := os.RemoveAll(filepath.Join(retained, pid)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ResolveHostPIDs(retained, localProc, observed); err == nil {
		t.Fatal("selected a process from another PID namespace")
	}
}

func writeProcess(t *testing.T, procRoot, pid, parent, pids, ns string) {
	t.Helper()
	path := filepath.Join(procRoot, pid)
	if err := os.MkdirAll(filepath.Join(path, "ns"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ns, filepath.Join(path, "ns/pid")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "status"), []byte("PPid:\t"+parent+"\nNSpid:\t"+pids+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProcessHandleSurvivesStopAndDetectsExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, "sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	files, err := OpenProcessHandles([]int{child.Process.Pid})
	if err != nil {
		t.Fatal(err)
	}
	defer files[0].Close()
	if err := unix.PidfdSendSignal(int(files[0].Fd()), unix.SIGSTOP, nil, 0); err != nil {
		t.Fatal(err)
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(child.Process.Pid, &status, unix.WUNTRACED, nil); err != nil || !status.Stopped() {
		t.Fatalf("child did not stop: %v, %v", status, err)
	}
	hostPIDs, err := ResolveHostPIDs("/proc", "/proc", []int{child.Process.Pid})
	if err != nil || len(hostPIDs) != 1 || hostPIDs[0] != child.Process.Pid {
		t.Fatalf("resolve stopped child: %v, %v", hostPIDs, err)
	}
	if err := CheckProcessHandles(files); err != nil {
		t.Fatalf("stopped child reported as exited: %v", err)
	}
	if err := unix.PidfdSendSignal(int(files[0].Fd()), unix.SIGKILL, nil, 0); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if err := CheckProcessHandles(files); err == nil {
		t.Fatal("accepted an exited and reaped target")
	}
	if err := files[0].Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckProcessHandles(files); err == nil {
		t.Fatal("accepted a closed target descriptor")
	}
}

func TestReadProcessFilesystemIDs(t *testing.T) {
	procRoot := t.TempDir()
	pidDir := filepath.Join(procRoot, "42")
	if err := os.Mkdir(pidDir, 0700); err != nil {
		t.Fatal(err)
	}
	status := "Uid:\t1000\t1001\t1002\t1003\nGid:\t2000\t2001\t2002\t2003\n"
	if err := os.WriteFile(filepath.Join(pidDir, "status"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}

	uid, gid, err := ReadProcessFilesystemIDs(procRoot, 42)
	if err != nil {
		t.Fatalf("ReadProcessFilesystemIDs() error = %v", err)
	}
	if uid != 1003 || gid != 2003 {
		t.Fatalf("ReadProcessFilesystemIDs() = %d:%d, want 1003:2003", uid, gid)
	}

	if err := os.WriteFile(filepath.Join(pidDir, "status"), []byte("Uid:\t1000\t1001\t1002\t1003\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadProcessFilesystemIDs(procRoot, 42); err == nil {
		t.Fatal("ReadProcessFilesystemIDs() accepted a status without Gid")
	}
}

func TestParseProcExitCode(t *testing.T) {
	tests := []struct {
		name     string
		statLine string
		wantCode int
		wantErr  bool
	}{
		{
			// Real /proc/<pid>/stat line (simplified). Fields after ")" start with state.
			// The last field (field 52) is exit_code.
			name:     "normal exit code 0",
			statLine: "123 (python3) S 1 123 123 0 -1 4194304 1000 0 0 0 100 50 0 0 20 0 1 0 1000 10000000 500 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0",
			wantCode: 0,
		},
		{
			name:     "non-zero exit code",
			statLine: "456 (bash) Z 1 456 456 0 -1 4194304 100 0 0 0 10 5 0 0 20 0 1 0 500 0 0 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 256",
			wantCode: 256, // signal 1 encoded as WaitStatus
		},
		{
			// Process names can contain spaces and parentheses.
			// The parser must use LastIndex(")") to handle this correctly.
			name:     "process name with spaces and parens",
			statLine: "789 (python3 -m vllm.entrypoints.openai.api_server (worker)) S 1 789 789 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 42",
			wantCode: 42,
		},
		{
			name:     "malformed line no closing paren",
			statLine: "123 (python3 S 1 123",
			wantErr:  true,
		},
		{
			name:     "empty string",
			statLine: "",
			wantErr:  true,
		},
		{
			name:     "only pid and comm, nothing after paren",
			statLine: "1 (init)",
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := ParseProcExitCode(tc.statLine)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got WaitStatus=%d", ws)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if int(ws) != tc.wantCode {
				t.Errorf("exit code = %d, want %d", int(ws), tc.wantCode)
			}
		})
	}
}

func TestReadProcessDetails(t *testing.T) {
	procRoot := t.TempDir()
	pid := 1018
	procDir := filepath.Join(procRoot, "1018")
	if err := os.MkdirAll(procDir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", procDir, err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "status"), []byte("Name:\tpython3\nPPid:\t0\nNSpid:\t2402711 1018\n"), 0644); err != nil {
		t.Fatalf("WriteFile(status): %v", err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "cmdline"), []byte("python3\x00-m\x00dynamo.vllm\x00"), 0644); err != nil {
		t.Fatalf("WriteFile(cmdline): %v", err)
	}

	details, err := ReadProcessDetails(procRoot, pid)
	if err != nil {
		t.Fatalf("ReadProcessDetails(%q, %d): %v", procRoot, pid, err)
	}
	if details.ObservedPID != 1018 {
		t.Fatalf("ObservedPID = %d, want 1018", details.ObservedPID)
	}
	if details.ParentPID != 0 {
		t.Fatalf("ParentPID = %d, want 0", details.ParentPID)
	}
	if details.OutermostPID != 2402711 {
		t.Fatalf("OutermostPID = %d, want 2402711", details.OutermostPID)
	}
	if details.InnermostPID != 1018 {
		t.Fatalf("InnermostPID = %d, want 1018", details.InnermostPID)
	}
	if len(details.NamespacePIDs) != 2 || details.NamespacePIDs[0] != 2402711 || details.NamespacePIDs[1] != 1018 {
		t.Fatalf("NamespacePIDs = %v, want [2402711 1018]", details.NamespacePIDs)
	}
	if details.Cmdline != "python3 -m dynamo.vllm" {
		t.Fatalf("Cmdline = %q, want %q", details.Cmdline, "python3 -m dynamo.vllm")
	}
}

func TestReadProcessDetailsOrDefault(t *testing.T) {
	details := ReadProcessDetailsOrDefault(t.TempDir(), 1234)
	if details.ObservedPID != 1234 {
		t.Fatalf("ObservedPID = %d, want 1234", details.ObservedPID)
	}
	if details.OutermostPID != 1234 {
		t.Fatalf("OutermostPID = %d, want 1234", details.OutermostPID)
	}
	if details.InnermostPID != 1234 {
		t.Fatalf("InnermostPID = %d, want 1234", details.InnermostPID)
	}
	if len(details.NamespacePIDs) != 1 || details.NamespacePIDs[0] != 1234 {
		t.Fatalf("NamespacePIDs = %v, want [1234]", details.NamespacePIDs)
	}
}

func TestReadProcessTable(t *testing.T) {
	procRoot := t.TempDir()
	writeEntry := func(pid int, status string, cmdline string) {
		t.Helper()
		procDir := filepath.Join(procRoot, strconv.Itoa(pid))
		if err := os.MkdirAll(procDir, 0755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", procDir, err)
		}
		if err := os.WriteFile(filepath.Join(procDir, "status"), []byte(status), 0644); err != nil {
			t.Fatalf("WriteFile(status): %v", err)
		}
		if err := os.WriteFile(filepath.Join(procDir, "cmdline"), []byte(cmdline), 0644); err != nil {
			t.Fatalf("WriteFile(cmdline): %v", err)
		}
	}

	writeEntry(768, "Name:\tworker\nPPid:\t1\nNSpid:\t2444000 768\n", "VLLM::Worker_TP0\x00")
	writeEntry(1, "Name:\tpython3\nPPid:\t0\nNSpid:\t2443990 1\n", "python3\x00-m\x00dynamo.vllm\x00")

	processes, err := ReadProcessTable(procRoot)
	if err != nil {
		t.Fatalf("ReadProcessTable(%q): %v", procRoot, err)
	}
	if len(processes) != 2 {
		t.Fatalf("len(ReadProcessTable(%q)) = %d, want 2", procRoot, len(processes))
	}
	if processes[0].InnermostPID != 1 || processes[1].InnermostPID != 768 {
		t.Fatalf("process order innermost PIDs = [%d %d], want [1 768]", processes[0].InnermostPID, processes[1].InnermostPID)
	}
}

func TestResolveManifestPIDsToObservedPIDs(t *testing.T) {
	processes := []ProcessDetails{
		{ObservedPID: 1, ParentPID: 0, OutermostPID: 1, InnermostPID: 1, NamespacePIDs: []int{1}, Cmdline: "sleep infinity"},
		{ObservedPID: 50, ParentPID: 0, OutermostPID: 50, InnermostPID: 50, NamespacePIDs: []int{50}, Cmdline: "nsrestore"},
		{ObservedPID: 74, ParentPID: 50, OutermostPID: 74, InnermostPID: 1, NamespacePIDs: []int{74, 1}, Cmdline: "python3 -m dynamo.vllm"},
		{ObservedPID: 80, ParentPID: 74, OutermostPID: 80, InnermostPID: 750, NamespacePIDs: []int{80, 750}, Cmdline: "VLLM::EngineCore"},
		{ObservedPID: 81, ParentPID: 74, OutermostPID: 81, InnermostPID: 749, NamespacePIDs: []int{81, 749}, Cmdline: "resource_tracker"},
	}

	resolved, err := ResolveManifestPIDsToObservedPIDs(processes, 74, []int{1, 750})
	if err != nil {
		t.Fatalf("ResolveManifestPIDsToObservedPIDs(...) returned error: %v", err)
	}
	if len(resolved) != 2 {
		t.Fatalf("len(resolved) = %d, want 2", len(resolved))
	}
	if resolved[0] != 74 || resolved[1] != 80 {
		t.Fatalf("resolved PIDs = %v, want [74 80]", resolved)
	}
}

func TestResolveManifestPIDsToObservedPIDsFailsWhenManifestPIDMissingFromRestoredSubtree(t *testing.T) {
	processes := []ProcessDetails{
		{ObservedPID: 1, ParentPID: 0, OutermostPID: 1, InnermostPID: 1, NamespacePIDs: []int{1}, Cmdline: "sleep infinity"},
		{ObservedPID: 50, ParentPID: 0, OutermostPID: 50, InnermostPID: 50, NamespacePIDs: []int{50}, Cmdline: "nsrestore"},
		{ObservedPID: 74, ParentPID: 50, OutermostPID: 74, InnermostPID: 1, NamespacePIDs: []int{74, 1}, Cmdline: "python3 -m dynamo.vllm"},
	}

	_, err := ResolveManifestPIDsToObservedPIDs(processes, 74, []int{1, 750})
	if err == nil {
		t.Fatal("ResolveManifestPIDsToObservedPIDs(...) unexpectedly succeeded")
	}
}

func TestResolveManifestPIDsToObservedPIDsFailsWhenNamespaceDepthIsNotTwo(t *testing.T) {
	processes := []ProcessDetails{
		{ObservedPID: 50, ParentPID: 0, OutermostPID: 50, InnermostPID: 50, NamespacePIDs: []int{50}, Cmdline: "nsrestore"},
		{ObservedPID: 74, ParentPID: 50, OutermostPID: 74, InnermostPID: 1, NamespacePIDs: []int{900, 74, 1}, Cmdline: "python3 -m dynamo.vllm"},
		{ObservedPID: 80, ParentPID: 74, OutermostPID: 80, InnermostPID: 750, NamespacePIDs: []int{900, 80, 750}, Cmdline: "VLLM::EngineCore"},
	}

	_, err := ResolveManifestPIDsToObservedPIDs(processes, 74, []int{1, 750})
	if err == nil {
		t.Fatal("ResolveManifestPIDsToObservedPIDs(...) unexpectedly succeeded")
	}
}

func writeKernelRelease(t *testing.T, procPath, content string) {
	t.Helper()
	dir := filepath.Join(procPath, "sys", "kernel")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "osrelease"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestReadKernelVersion(t *testing.T) {
	procPath := t.TempDir()
	writeKernelRelease(t, procPath, "5.15.0-119-generic\n")

	got, err := ReadKernelVersion(procPath)
	if err != nil {
		t.Fatalf("ReadKernelVersion: %v", err)
	}
	if got != "5.15.0-119-generic" {
		t.Fatalf("ReadKernelVersion() = %q, want 5.15.0-119-generic", got)
	}
}

// An unreadable or blank release is reported rather than passed on: a value
// recorded as the empty string would be indistinguishable from one this agent
// version never recorded at all.
func TestReadKernelVersionRejectsWhatItCannotRead(t *testing.T) {
	if _, err := ReadKernelVersion(t.TempDir()); err == nil {
		t.Fatal("expected an error when the host proc mount has no osrelease")
	}

	procPath := t.TempDir()
	writeKernelRelease(t, procPath, "\n")
	if _, err := ReadKernelVersion(procPath); err == nil {
		t.Fatal("expected an error when osrelease is blank")
	}
}
