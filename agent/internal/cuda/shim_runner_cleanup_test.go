// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// hangingHelperScript stands in for a cuda-checkpoint-helper that never returns. It
// needs no GPU: it forks a worker, records the worker's PID, and waits on it.
const hangingHelperScript = "#!/bin/sh\nsleep 300 &\necho \"$!\" > \"$CHILD_PID_FILE\"\nwait\n"

func writeFakeHelper(t *testing.T, script string) (helper, childPIDFile string) {
	t.Helper()
	dir := t.TempDir()
	helper = filepath.Join(dir, "cuda-checkpoint-helper")
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	childPIDFile = filepath.Join(dir, "child-pid")
	t.Setenv("CHILD_PID_FILE", childPIDFile)
	return helper, childPIDFile
}

func waitForChildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("fake helper did not record its child PID")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processExited reports whether pid is gone or only waiting to be reaped. A killed
// orphan can stay a zombie when PID 1 of a test container never reaps it.
func processExited(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func requireChildExits(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(time.Second)
	for !processExited(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("helper child %d is still running after cleanup", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHelperParentCancellationKillsChild(t *testing.T) {
	for _, action := range []string{actionLock, actionCheckpoint} {
		t.Run(action, func(t *testing.T) {
			helper, childPIDFile := writeFakeHelper(t, hangingHelperScript)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			childPID := make(chan int, 1)
			go func() {
				pid := waitForChildPID(t, childPIDFile)
				cancel()
				childPID <- pid
			}()

			started := time.Now()
			err := runCheckpointHelper(ctx, 11, action, helper, []string{"--action", action}, true, logr.Discard())
			elapsed := time.Since(started)
			if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
				t.Fatalf("runCheckpointHelper() error = %v, want context canceled", err)
			}
			if elapsed > helperWaitDelay+time.Second {
				t.Fatalf("canceled %s helper took %s", action, elapsed)
			}
			requireChildExits(t, <-childPID)
		})
	}
}

func TestCheckpointTimeoutKillsChild(t *testing.T) {
	helper, childPIDFile := writeFakeHelper(t, hangingHelperScript)
	original := cudaCheckpointTimeout
	cudaCheckpointTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cudaCheckpointTimeout = original })

	started := time.Now()
	err := runCheckpointHelper(context.Background(), 11, actionCheckpoint, helper, []string{"--action", actionCheckpoint}, true, logr.Discard())
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("runCheckpointHelper() error = %v, want deadline exceeded", err)
	}
	if elapsed > helperWaitDelay+time.Second {
		t.Fatalf("timed-out checkpoint helper took %s", elapsed)
	}
	requireChildExits(t, waitForChildPID(t, childPIDFile))
}

// Restore and unlock keep the caller's deadline only: no extra timeout, no process group.
func TestRestoreCancellationIsBounded(t *testing.T) {
	helper, childPIDFile := writeFakeHelper(t, "#!/bin/sh\necho \"$$\" > \"$CHILD_PID_FILE\"\nexec sleep 300\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	helperPID := make(chan int, 1)
	go func() {
		pid := waitForChildPID(t, childPIDFile)
		cancel()
		helperPID <- pid
	}()

	started := time.Now()
	err := runCheckpointHelper(ctx, 11, actionRestore, helper, []string{"--action", actionRestore}, false, logr.Discard())
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("runCheckpointHelper() error = %v, want context canceled", err)
	}
	if elapsed > helperWaitDelay+time.Second {
		t.Fatalf("canceled restore helper took %s", elapsed)
	}
	requireChildExits(t, <-helperPID)
}

// A helper that exits while a descendant still holds its output pipes must not block
// the agent for as long as that descendant lives.
func TestHelperExitWithOpenPipesIsBounded(t *testing.T) {
	helper, childPIDFile := writeFakeHelper(t, "#!/bin/sh\nsleep 300 &\necho \"$!\" > \"$CHILD_PID_FILE\"\n")
	t.Cleanup(func() {
		if data, err := os.ReadFile(childPIDFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	started := time.Now()
	err := runCheckpointHelper(context.Background(), 11, actionCheckpoint, helper, []string{"--action", actionCheckpoint}, true, logr.Discard())
	if elapsed := time.Since(started); elapsed > helperWaitDelay+time.Second {
		t.Fatalf("helper with a pipe-holding descendant took %s (error: %v)", elapsed, err)
	}
}

func TestHelperThatExitsReturnsWithoutWaitingForLibcuda(t *testing.T) {
	helper, _ := writeFakeHelper(t, "#!/bin/sh\nexit 0\n")
	started := time.Now()
	if err := runCheckpointHelper(context.Background(), 11, actionUnlock, helper, []string{"--action", actionUnlock}, false, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("helper that exited at once took %s", elapsed)
	}
}

func TestHelperFailureKeepsExitStatusAndOutput(t *testing.T) {
	helper, _ := writeFakeHelper(t, "#!/bin/sh\necho 'lock refused' >&2\nexit 3\n")
	err := runCheckpointHelper(context.Background(), 11, actionLock, helper, []string{"--action", actionLock}, true, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "lock refused") {
		t.Fatalf("runCheckpointHelper() error = %v, want exit status 3 and the helper output", err)
	}
}
