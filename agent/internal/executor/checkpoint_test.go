// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/go-logr/logr"
)

func TestTerminateCUDAProcessesAfterCheckpointFailurePropagatesCleanupError(t *testing.T) {
	var attempted []int
	err := terminateCUDAProcessesAfterCheckpointFailure(
		[]int{41, 42},
		logr.Discard(),
		func(_ logr.Logger, pid int, signal syscall.Signal, reason string) error {
			attempted = append(attempted, pid)
			if signal != syscall.SIGKILL || reason != "CUDA checkpoint failed" {
				t.Fatalf("signal call = (%d, %q), want SIGKILL and checkpoint reason", signal, reason)
			}
			if pid == 41 {
				return errors.New("permission denied")
			}
			return nil
		},
	)
	if len(attempted) != 2 || attempted[0] != 41 || attempted[1] != 42 {
		t.Fatalf("attempted PIDs = %v, want [41 42]", attempted)
	}
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("cleanup error = %v, want propagated signal failure", err)
	}
}
