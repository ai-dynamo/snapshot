// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"strconv"
	"testing"
)

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

func TestResolveHostPIDsRejectsUnsupportedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pids      string
		namespace string
		duplicate bool
	}{
		{"host namespace", "901", "/proc/self/ns/pid", false},
		{"nested namespace", "901 100 12", "/proc/self/ns/pid", false},
		{"wrong host PID", "999 12", "/proc/self/ns/pid", false},
		{"other namespace", "901 12", "/proc/self/ns/mnt", false},
		{"ambiguous PID", "901 12", "/proc/self/ns/pid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeProcess := func(pid, pids string) {
				t.Helper()
				path := root + "/" + pid
				if err := os.MkdirAll(path+"/ns", 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(tc.namespace, path+"/ns/pid"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+"/status", []byte("PPid:\t1\nNSpid:\t"+pids+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeProcess("901", tc.pids)
			if tc.duplicate {
				writeProcess("902", "902 12")
			}
			if _, err := ResolveHostPIDs(root, []int{12}); err == nil {
				t.Fatal("accepted unsupported or ambiguous process identity")
			}
		})
	}
}

func TestResolveHostPIDsInCRIUNamespace(t *testing.T) {
	if os.Getenv("SNAPSHOT_PID_NAMESPACE_TEST_CHILD") == "1" {
		time.Sleep(time.Minute)
		return
	}
	child := exec.Command(os.Args[0], "-test.run=^TestResolveHostPIDsInCRIUNamespace$")
	child.Env = append(os.Environ(), "SNAPSHOT_PID_NAMESPACE_TEST_CHILD=1")
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID}
	if err := child.Start(); err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("PID namespace creation requires CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	root := t.TempDir()
	process := root + "/901"
	if err := os.MkdirAll(process+"/ns", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/proc/"+strconv.Itoa(child.Process.Pid)+"/ns/pid", process+"/ns/pid"); err != nil {
		t.Fatal(err)
	}
	status := "PPid: 1\nNSpid: 901 " + strconv.Itoa(child.Process.Pid) + " 1\n"
	if err := os.WriteFile(process+"/status", []byte(status), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveHostPIDs(root, []int{child.Process.Pid})
	if err != nil || len(got) != 1 || got[0] != 901 {
		t.Fatalf("host PIDs = %v, %v, want [901]", got, err)
	}
}
