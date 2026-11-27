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
)

const (
	helperWaitDelay = 2 * time.Second

	actionLock       = "lock"
	actionCheckpoint = "checkpoint"
	actionRestore    = "restore"
	actionUnlock     = "unlock"
)

var cudaCheckpointHelperBinary = DefaultHelperBinaryPath

func lock(ctx context.Context, pid int, log logr.Logger) error {
	return runAction(ctx, pid, actionLock, "", DefaultHelperBinaryPath, true, log)
}

func checkpoint(ctx context.Context, pid int, log logr.Logger) error {
	return runAction(ctx, pid, actionCheckpoint, "", DefaultHelperBinaryPath, true, log)
}

func restoreProcess(ctx context.Context, pid int, deviceMap, helperBinaryPath string, log logr.Logger) error {
	return runAction(ctx, pid, actionRestore, deviceMap, helperBinaryPath, false, log)
}

func unlock(ctx context.Context, pid int, helperBinaryPath string, log logr.Logger) error {
	return runAction(ctx, pid, actionUnlock, "", helperBinaryPath, false, log)
}

func getState(ctx context.Context, pid int, helperBinaryPath string) (string, error) {
	cmd := exec.CommandContext(ctx, helperBinaryPath, "--get-state", "--pid", strconv.Itoa(pid))
	output, err := cmd.CombinedOutput()
	state := strings.TrimSpace(string(output))
	if err != nil {
		return "", fmt.Errorf("cuda-checkpoint-helper --get-state failed for pid %d: %w (output: %s)", pid, err, state)
	}
	if state == "" {
		return "", fmt.Errorf("cuda-checkpoint-helper --get-state returned empty state for pid %d", pid)
	}
	return state, nil
}

func runAction(ctx context.Context, pid int, action, deviceMap, helperBinaryPath string, ownProcessGroup bool, log logr.Logger) error {
	args := []string{"--action", action, "--pid", strconv.Itoa(pid)}
	if action == actionLock {
		args = append(args, "--timeout", strconv.FormatInt(int64(cudaLockTimeout/time.Millisecond), 10))
	}
	if action == actionRestore && deviceMap != "" {
		args = append(args, "--device-map", deviceMap)
	}
	return runCheckpointHelper(ctx, pid, action, helperBinaryPath, args, ownProcessGroup, log)
}
