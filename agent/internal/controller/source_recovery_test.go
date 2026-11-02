// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	snapshottypes "github.com/ai-dynamo/snapshot/agent/internal/types"
)

// recoveryController counts kills and records what revival was asked to do, so
// a test can say what became of the source without a GPU.
func recoveryController(reviveErr error) (*NodeController, *int, *[]int) {
	kills := 0
	var revived []int
	w := &NodeController{
		log:          logr.Discard(),
		config:       &snapshottypes.AgentConfig{},
		hostProcPath: "/proc",
		reviveGPUProcessesFn: func(ctx context.Context, pids []int, _ string, _ logr.Logger) error {
			// The real one execs the checkpoint helper with this context, so a
			// dead one reaches the helper as a failure rather than a revival.
			if err := ctx.Err(); err != nil {
				return err
			}
			revived = append(revived, pids...)
			return reviveErr
		},
		sendSignalFn: func(logr.Logger, int, syscall.Signal, string) error {
			kills++
			return nil
		},
	}
	return w, &kills, &revived
}

// A failed dump leaves the workload suspended but intact. Killing it destroys a
// workload that could have carried on, so it is killed only when it cannot be
// put back.
func TestReviveSourceRefusesWhatItCannotPutBack(t *testing.T) {
	for _, tc := range []struct {
		name         string
		containerPID int
		gpuPIDs      []int
		reviveErr    error
	}{
		{name: "the process tree is gone", containerPID: 0, gpuPIDs: []int{4242}},
		{
			name:         "the GPU state cannot be recovered",
			containerPID: os.Getpid(),
			gpuPIDs:      []int{4242},
			reviveErr:    errors.New("process is failed"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, _ := recoveryController(tc.reviveErr)

			reason, ok := w.reviveSource(context.Background(), logr.Discard(), tc.containerPID, tc.gpuPIDs)

			assert.False(t, ok)
			assert.NotEmpty(t, reason, "a source that cannot be revived must say why")
		})
	}
}

// This process is running and untraced, so it stands in for a tree CRIU
// released: the GPUs go back and nothing is killed.
func TestReviveSourceLeavesAReleasedTreeRunning(t *testing.T) {
	w, kills, revived := recoveryController(nil)

	w.recoverSourceAfterFailedDump(context.Background(), logr.Discard(), os.Getpid(), []int{4242})

	assert.Equal(t, 0, *kills, "a workload that was put back must not be killed")
	assert.Equal(t, []int{4242}, *revived, "the capture's own GPU processes must be handed back")
}

// The kill is the fallback, not the default: it fires exactly when revival
// could not put the workload back.
func TestRecoverSourceKillsOnlyWhatCannotBeRevived(t *testing.T) {
	w, kills, _ := recoveryController(errors.New("process is failed"))

	w.recoverSourceAfterFailedDump(context.Background(), logr.Discard(), os.Getpid(), []int{4242})

	assert.Equal(t, 1, *kills, "a source that cannot be revived must not be left hanging")
}

// A CPU-only capture has no GPUs to hand back, and must still be left running
// rather than killed.
func TestReviveSourceWithoutGPUsLeavesTheTreeRunning(t *testing.T) {
	w, kills, revived := recoveryController(errors.New("must not be called"))

	reason, ok := w.reviveSource(context.Background(), logr.Discard(), os.Getpid(), nil)

	require.True(t, ok, "a released CPU-only tree needs no revival: %s", reason)
	assert.Empty(t, *revived)
	assert.Equal(t, 0, *kills)
}

// Recovery must outlive the capture: a dump that failed on its deadline leaves
// that context dead, and the GPUs still have to go back.
func TestRecoverSourceSurvivesACancelledCapture(t *testing.T) {
	w, kills, revived := recoveryController(nil)
	captureCtx, cancel := context.WithCancel(context.Background())
	cancel()

	w.recoverSourceAfterFailedDump(captureCtx, logr.Discard(), os.Getpid(), []int{4242})

	assert.Equal(t, []int{4242}, *revived, "a dead capture context must not skip the revival")
	assert.Equal(t, 0, *kills)
}
