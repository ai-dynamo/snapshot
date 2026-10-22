// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
)

// States reported by the checkpoint helper.
const (
	StateRunning      = "running"
	StateLocked       = "locked"
	StateCheckpointed = "checkpointed"
	StateFailed       = "failed"
)

// ProcessStates reads the GPU state of each process.
func ProcessStates(ctx context.Context, pids []int, helperBinaryPath string) (map[int]string, error) {
	states := make(map[int]string, len(pids))
	for _, pid := range pids {
		state, err := getState(ctx, pid, helperBinaryPath)
		if err != nil {
			return nil, err
		}
		states[pid] = state
	}
	return states, nil
}

// ReviveProcessTree hands a suspended workload back the GPUs it still holds,
// so a capture that could not be completed leaves it running rather than dead.
//
// Checkpointing is a reversible state machine, and a tree can be part way
// through it: a suspend that failed leaves some processes merely locked while
// others are already checkpointed. Each process is therefore dispatched on its
// own state, and every one is attempted even after another fails — stopping at
// the first error would leave the rest of the tree locked, which is worse than
// not having tried. No device remapping is needed, because the workload is
// being handed back the GPUs it never released.
func ReviveProcessTree(ctx context.Context, pids []int, helperBinaryPath string, log logr.Logger) error {
	var failures []error
	for _, pid := range pids {
		if err := reviveProcess(ctx, pid, helperBinaryPath, log); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	// The workload is only revived if every process is back to running: one left
	// behind makes the tree unusable to whatever holds the other processes.
	for _, pid := range pids {
		state, err := getState(ctx, pid, helperBinaryPath)
		if err != nil {
			failures = append(failures, fmt.Errorf("confirm process %d resumed: %w", pid, err))
			continue
		}
		if state != StateRunning {
			failures = append(failures, fmt.Errorf("process %d is %q after revival, not %q", pid, state, StateRunning))
		}
	}
	return errors.Join(failures...)
}

// reviveProcess walks one process back to running from wherever it stopped.
func reviveProcess(ctx context.Context, pid int, helperBinaryPath string, log logr.Logger) error {
	state, err := getState(ctx, pid, helperBinaryPath)
	if err != nil {
		return fmt.Errorf("read GPU state of process %d: %w", pid, err)
	}
	switch state {
	case StateRunning:
		return nil
	case StateLocked:
		if err := unlock(ctx, pid, helperBinaryPath, log); err != nil {
			return fmt.Errorf("unlock process %d: %w", pid, err)
		}
		return nil
	case StateCheckpointed:
		// Restoring in place, so the device map stays empty: these are the same
		// GPUs the process was using.
		if err := restoreProcess(ctx, pid, "", helperBinaryPath, log); err != nil {
			return fmt.Errorf("restore process %d: %w", pid, err)
		}
		if err := unlock(ctx, pid, helperBinaryPath, log); err != nil {
			return fmt.Errorf("unlock process %d after restore: %w", pid, err)
		}
		return nil
	default:
		// StateFailed, or anything the helper does not name: the driver cannot
		// account for this process, so nothing can hand its GPUs back.
		return fmt.Errorf("process %d is %q and cannot be revived", pid, state)
	}
}
