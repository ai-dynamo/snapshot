// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
)

const (
	// CheckpointHelperBinary is opened by the agent and inherited by nsrestore
	// for CUDA work inside the target mount namespace.
	CheckpointHelperBinary = "/usr/local/bin/cuda-checkpoint-helper"

	actionLock       = "lock"
	actionCheckpoint = "checkpoint"
	actionRestore    = "restore"
	actionUnlock     = "unlock"
)

func lock(ctx context.Context, pid int, log logr.Logger) error {
	return runAction(ctx, CheckpointHelperBinary, pid, actionLock, "", log)
}

func checkpoint(ctx context.Context, pid int, log logr.Logger) error {
	return runAction(ctx, CheckpointHelperBinary, pid, actionCheckpoint, "", log)
}

func restoreProcess(ctx context.Context, helperBinary string, pid int, deviceMap string, log logr.Logger) error {
	return runAction(ctx, helperBinary, pid, actionRestore, deviceMap, log)
}

func unlock(ctx context.Context, helperBinary string, pid int, log logr.Logger) error {
	return runAction(ctx, helperBinary, pid, actionUnlock, "", log)
}

func runAction(ctx context.Context, helperBinary string, pid int, action, deviceMap string, log logr.Logger) error {
	args := []string{"--action", action, "--pid", strconv.Itoa(pid)}
	if action == actionRestore && deviceMap != "" {
		args = append(args, "--device-map", deviceMap)
	}
	cmd := exec.CommandContext(ctx, helperBinary, args...)
	details := snapshotruntime.ProcessDetails{
		ObservedPID:   pid,
		OutermostPID:  pid,
		InnermostPID:  pid,
		NamespacePIDs: []int{pid},
	}
	if process, err := snapshotruntime.ReadProcessDetails("/proc", pid); err == nil {
		details = process
	}
	start := time.Now()
	output, err := cmd.CombinedOutput()
	duration := time.Since(start)
	out := strings.TrimSpace(string(output))
	if err != nil {
		log.Error(err, "cuda-checkpoint-helper command failed",
			"pid", pid,
			"outermost_pid", details.OutermostPID,
			"innermost_pid", details.InnermostPID,
			"cmdline", details.Cmdline,
			"action", action,
			"duration", duration,
			"output", out,
		)
		return fmt.Errorf("cuda-checkpoint-helper %v failed for pid %d after %s: %w (output: %s)", args, pid, duration, err, out)
	}
	log.V(1).Info("cuda-checkpoint-helper command succeeded",
		"pid", pid,
		"outermost_pid", details.OutermostPID,
		"innermost_pid", details.InnermostPID,
		"cmdline", details.Cmdline,
		"action", action,
		"duration", duration,
		"output", out,
	)
	return nil
}
