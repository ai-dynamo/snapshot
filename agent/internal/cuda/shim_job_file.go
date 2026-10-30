// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"strconv"
	"time"

	"github.com/go-logr/logr"
)

func lockWithJobFile(ctx context.Context, pid int, jobFile string, log logr.Logger) error {
	if jobFile == "" {
		return lock(ctx, pid, log)
	}
	return runActionWithJobFile(ctx, pid, actionLock, jobFile, log)
}

func checkpointWithJobFile(ctx context.Context, pid int, jobFile string, log logr.Logger) error {
	if jobFile == "" {
		return checkpoint(ctx, pid, log)
	}
	return runActionWithJobFile(ctx, pid, actionCheckpoint, jobFile, log)
}

func runActionWithJobFile(ctx context.Context, pid int, action, jobFile string, log logr.Logger) error {
	args := []string{"--action", action, "--pid", strconv.Itoa(pid), "--job-file", jobFile}
	if action == actionLock {
		args = append(args, "--timeout", strconv.FormatInt(int64(cudaLockTimeout/time.Millisecond), 10))
	}
	return runCheckpointHelper(ctx, pid, action, cudaCheckpointHelperBinary, args, true, log)
}
