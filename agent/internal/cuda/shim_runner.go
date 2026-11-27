// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

var (
	cudaLockTimeout       = 10 * time.Second
	cudaLockKillMargin    = 5 * time.Second
	cudaCheckpointTimeout = 10 * time.Minute
)

const libcudaMapWait = 2 * time.Second

func helperActionTimeout(action string) time.Duration {
	switch action {
	case actionLock:
		return cudaLockTimeout + cudaLockKillMargin
	case actionCheckpoint:
		return cudaCheckpointTimeout
	default:
		return 0
	}
}

func runCheckpointHelper(ctx context.Context, targetPID int, action, helperPath string, args []string, ownProcessGroup bool, log logr.Logger) error {
	timeout := helperActionTimeout(action)
	actionCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		actionCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(actionCtx, helperPath, args...)
	if ownProcessGroup {
		snapshotruntime.SetProcessGroupCancellation(cmd, helperWaitDelay)
	}
	details := snapshotruntime.ProcessDetails{
		ObservedPID:   targetPID,
		OutermostPID:  targetPID,
		InnermostPID:  targetPID,
		NamespacePIDs: []int{targetPID},
	}
	if process, err := snapshotruntime.ReadProcessDetails("/proc", targetPID); err == nil {
		details = process
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cuda-checkpoint-helper action %s for pid %d: %w", action, targetPID, err)
	}

	start := time.Now()
	// Wait concurrently so a helper that exits before mapping libcuda is not held for libcudaMapWait.
	exited := make(chan struct{})
	var err error
	go func() {
		err = cmd.Wait()
		close(exited)
	}()
	libcudaPath, libcudaVersion := readMappedLibcudaAfterStart(cmd.Process.Pid, exited)
	log.Info("cuda-checkpoint-helper command started",
		"pid", cmd.Process.Pid,
		"target_pid", targetPID,
		"outermost_pid", details.OutermostPID,
		"innermost_pid", details.InnermostPID,
		"cmdline", details.Cmdline,
		"action", action,
		"args", args,
		"timeout", timeout,
		"libcuda_path", libcudaPath,
		"libcuda_version", libcudaVersion,
	)

	<-exited
	duration := time.Since(start)
	out := strings.TrimSpace(output.String())
	if err != nil {
		if actionCtx.Err() != nil {
			err = actionCtx.Err()
		}
		if !(action == actionRestore && err == context.Canceled) {
			log.Error(err, "cuda-checkpoint-helper command failed",
				"pid", cmd.Process.Pid,
				"target_pid", targetPID,
				"outermost_pid", details.OutermostPID,
				"innermost_pid", details.InnermostPID,
				"cmdline", details.Cmdline,
				"action", action,
				"duration", duration,
				"timed_out", actionCtx.Err() == context.DeadlineExceeded,
				"libcuda_path", libcudaPath,
				"libcuda_version", libcudaVersion,
				"output", out,
			)
		}
		return fmt.Errorf("cuda-checkpoint-helper %v failed for pid %d after %s: %w (output: %s)", args, targetPID, duration, err, out)
	}
	log.Info("cuda-checkpoint-helper command finished",
		"pid", cmd.Process.Pid,
		"target_pid", targetPID,
		"outermost_pid", details.OutermostPID,
		"innermost_pid", details.InnermostPID,
		"cmdline", details.Cmdline,
		"action", action,
		"duration", duration,
		"timed_out", false,
		"libcuda_path", libcudaPath,
		"libcuda_version", libcudaVersion,
	)
	return nil
}

func readMappedLibcudaAfterStart(pid int, exited <-chan struct{}) (string, string) {
	deadline := time.Now().Add(libcudaMapWait)
	for {
		if path, version := readMappedLibcuda("/proc", pid); path != "" {
			return path, version
		}
		if time.Now().After(deadline) {
			return "unavailable", "unavailable"
		}
		select {
		case <-exited:
			return "unavailable", "unavailable"
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func readMappedLibcuda(procRoot string, pid int) (string, string) {
	if pid <= 0 {
		return "", ""
	}
	content, err := os.ReadFile(filepath.Join(procRoot, fmt.Sprint(pid), "maps"))
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || !strings.Contains(fields[5], "libcuda.so") {
			continue
		}
		path := strings.TrimSuffix(strings.ReplaceAll(fields[5], "\\040", " "), " (deleted)")
		base := filepath.Base(path)
		version, ok := strings.CutPrefix(base, "libcuda.so.")
		if !ok || version == "" || strings.Count(version, ".") < 2 {
			version = "unknown"
		}
		return path, version
	}
	return "", ""
}
