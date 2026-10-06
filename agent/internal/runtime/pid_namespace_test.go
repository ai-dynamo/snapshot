// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPIDAtNamespaceDepth(t *testing.T) {
	for _, test := range []struct {
		name        string
		pids        []int
		depth, want int
	}{
		{"container", []int{901, 12}, 0, 12},
		{"nested source", []int{901, 112, 12}, 1, 112},
		{"host", []int{901, 112, 12}, 2, 901},
		{"ancestry mismatch", []int{901, 12}, 2, 0},
		{"invalid PID", []int{901, 0}, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := pidAtNamespaceDepth(test.pids, test.depth)
			if got != test.want || (err != nil) != (test.want == 0) {
				t.Fatalf("PID = %d, %v, want %d", got, err, test.want)
			}
		})
	}
}

func TestReadProcessPIDInNamespace(t *testing.T) {
	namespace, err := os.Open("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	got, err := ReadProcessPIDInNamespace("/proc", os.Getpid(), namespace)
	if err != nil || got != os.Getpid() {
		t.Fatalf("PID = %d, %v, want %d", got, err, os.Getpid())
	}
	wrong, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err := ReadProcessPIDInNamespace("/proc", os.Getpid(), wrong); err == nil {
		t.Fatal("accepted a mount namespace as a PID namespace")
	}
}

func TestReadProcessPIDInNamespaceNested(t *testing.T) {
	if os.Getenv("SNAPSHOT_PID_NAMESPACE_TEST_CHILD") == "1" {
		time.Sleep(time.Minute)
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestReadProcessPIDInNamespaceNested$")
	child.Env = append(os.Environ(), "SNAPSHOT_PID_NAMESPACE_TEST_CHILD=1")
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID}
	if err := child.Start(); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("PID namespace creation requires CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	parentNamespace, err := os.Open("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	defer parentNamespace.Close()
	got, err := ReadProcessPIDInNamespace("/proc", child.Process.Pid, parentNamespace)
	if err != nil || got != child.Process.Pid {
		t.Fatalf("nested PID = %d, %v, want parent-visible %d", got, err, child.Process.Pid)
	}
	childNamespace, err := os.Open("/proc/" + strconv.Itoa(child.Process.Pid) + "/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	defer childNamespace.Close()
	if _, err := ReadProcessPIDInNamespace("/proc", os.Getpid(), childNamespace); err == nil {
		t.Fatal("accepted process outside pinned descendant namespace")
	}
}

func TestResolveHostPIDsUsesRetainedProcDirectory(t *testing.T) {
	root := t.TempDir() + "/proc"
	for hostPID, localPID := range map[int]int{901: 12, 902: 24} {
		process := root + "/" + strconv.Itoa(hostPID)
		if err := os.MkdirAll(process+"/ns", 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/proc/self/ns/pid", process+"/ns/pid"); err != nil {
			t.Fatal(err)
		}
		status := "PPid:\t1\nNSpid:\t" + strconv.Itoa(hostPID) + "\t" + strconv.Itoa(localPID) + "\n"
		if err := os.WriteFile(process+"/status", []byte(status), 0600); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
	retained := "/proc/self/fd/" + strconv.Itoa(int(directory.Fd()))
	got, err := ResolveHostPIDs(retained, []int{24, 12})
	if err != nil || len(got) != 2 || got[0] != 902 || got[1] != 901 {
		t.Fatalf("host PIDs = %v, %v, want [902 901]", got, err)
	}
	for _, pids := range [][]int{{999}, {12, 12}, {0}} {
		if _, err := ResolveHostPIDs(retained, pids); err == nil {
			t.Fatalf("accepted invalid targets %v", pids)
		}
	}
}
