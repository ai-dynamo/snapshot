// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestHelperActionTimeouts(t *testing.T) {
	if got := helperActionTimeout(actionLock); got != 15*time.Second {
		t.Fatalf("outer lock deadline = %s, want 15s", got)
	}
	if got := helperActionTimeout(actionCheckpoint); got != 10*time.Minute {
		t.Fatalf("checkpoint timeout = %s, want 10m", got)
	}
	if got := helperActionTimeout(actionRestore); got != 0 {
		t.Fatalf("restore timeout = %s, want no additional timeout", got)
	}
}

func TestLockThatNeverReturnsIsBounded(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "args")
	helper := filepath.Join(dir, "cuda-checkpoint-helper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TRACE_FILE\"\nsleep 300 &\nwait\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACE_FILE", trace)

	originalLockTimeout, originalKillMargin := cudaLockTimeout, cudaLockKillMargin
	cudaLockTimeout, cudaLockKillMargin = 200*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() {
		cudaLockTimeout, cudaLockKillMargin = originalLockTimeout, originalKillMargin
	})

	started := time.Now()
	err := runCheckpointHelper(context.Background(), 11, actionLock, helper,
		[]string{"--action", actionLock, "--pid", "11", "--timeout", "200"}, true, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("runCheckpointHelper() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > helperWaitDelay+time.Second {
		t.Fatalf("lock helper took %s after its bounded deadline", elapsed)
	}
	args, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--timeout\n200") {
		t.Fatalf("helper args = %q, want the bounded driver lock timeout", args)
	}
}

func TestReadMappedLibcuda(t *testing.T) {
	procRoot := t.TempDir()
	mapsDir := filepath.Join(procRoot, "456")
	if err := os.Mkdir(mapsDir, 0700); err != nil {
		t.Fatal(err)
	}
	content := "7f000000-7f001000 r-xp 00000000 08:01 12 /usr/lib/libc.so.6\n" +
		"7f001000-7f002000 r-xp 00000000 08:01 13 /usr/lib/nvidia/libcuda.so.565.57.01\n"
	if err := os.WriteFile(filepath.Join(mapsDir, "maps"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	path, version := readMappedLibcuda(procRoot, 456)
	if path != "/usr/lib/nvidia/libcuda.so.565.57.01" || version != "565.57.01" {
		t.Fatalf("mapped libcuda = (%q, %q)", path, version)
	}
	if err := os.WriteFile(filepath.Join(mapsDir, "maps"), []byte("7f001000-7f002000 r-xp 00000000 08:01 13 /usr/lib/nvidia/libcuda.so.1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if path, version := readMappedLibcuda(procRoot, 456); path != "/usr/lib/nvidia/libcuda.so.1" || version != "unknown" {
		t.Fatalf("unversioned mapped libcuda = (%q, %q)", path, version)
	}
}

func TestRunActionPassesDriverTimeoutToLock(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "args")
	helper := filepath.Join(dir, "cuda-checkpoint-helper")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$TRACE_FILE\"\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACE_FILE", trace)
	if err := runAction(context.Background(), 321, actionLock, "", helper, false, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(args); !strings.Contains(got, "--action lock --pid 321 --timeout 10000") {
		t.Fatalf("helper args = %q; want the 10s driver lock timeout", got)
	}
}

func TestRunActionWithJobFilePassesDriverTimeoutToLock(t *testing.T) {
	dir := t.TempDir()
	trace := filepath.Join(dir, "args")
	helper := filepath.Join(dir, "cuda-checkpoint-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$TRACE_FILE\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	originalHelper := cudaCheckpointHelperBinary
	cudaCheckpointHelperBinary = helper
	t.Cleanup(func() { cudaCheckpointHelperBinary = originalHelper })
	t.Setenv("TRACE_FILE", trace)
	jobFile := filepath.Join(dir, "job-file")
	if err := runActionWithJobFile(context.Background(), 654, actionLock, jobFile, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(args); !strings.Contains(got, "--action lock --pid 654 --job-file "+jobFile+" --timeout 10000") {
		t.Fatalf("helper args = %q; want the job-file lock timeout", got)
	}
}

func TestCheckpointHelperCancellationKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "cuda-checkpoint-helper")
	childStarted := filepath.Join(dir, "child-started")
	marker := filepath.Join(dir, "surviving-child")
	script := "#!/bin/sh\n(printf x > \"$STARTED_FILE\"; sleep 0.5; printf x > \"$MARKER_FILE\") &\nwait\n"
	if err := os.WriteFile(helper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STARTED_FILE", childStarted)
	t.Setenv("MARKER_FILE", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := runCheckpointHelper(ctx, 42, actionCheckpoint, helper, []string{"--action", actionCheckpoint}, true, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("runCheckpointHelper() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > helperWaitDelay+time.Second {
		t.Fatalf("canceled helper took %s", elapsed)
	}
	startDeadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(childStarted); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(startDeadline) {
			t.Fatal("descendant did not start before cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Wait beyond the descendant's delayed write deadline so this proves it was
	// terminated, rather than merely not having written the marker yet.
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived process-group cancellation; marker stat error = %v", err)
	}
}
