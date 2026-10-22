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

// recoveryController wires the GPU seams and counts kills, so a test can say
// what became of the source without a GPU or a live process tree.
func recoveryController(t *testing.T, gpuPIDs []int, reviveErr error) (*NodeController, *int) {
	t.Helper()
	kills := 0
	w := &NodeController{
		log:    logr.Discard(),
		config: &snapshottypes.AgentConfig{},
		filterCUDAProcessesFn: func(context.Context, []int, logr.Logger) []int {
			return gpuPIDs
		},
		reviveGPUProcessesFn: func(context.Context, []int, string, logr.Logger) error {
			return reviveErr
		},
		sendSignalFn: func(logr.Logger, int, syscall.Signal, string) error {
			kills++
			return nil
		},
	}
	return w, &kills
}

// A failed dump leaves the workload suspended but intact. Killing it destroys a
// workload that could have carried on, so it is killed only when it cannot be
// put back.
func TestReviveSourceRefusesWhatItCannotPutBack(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gpuPIDs   []int
		reviveErr error
	}{
		{"the GPU state cannot be recovered", []int{4242}, errors.New("process is failed")},
		{"there are no GPUs to hand back, and no tree either", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := recoveryController(t, tc.gpuPIDs, tc.reviveErr)

			// PID 0 has no process tree, so the tree lookup finds nothing.
			reason, ok := w.reviveSource(context.Background(), logr.Discard(), 0)

			assert.False(t, ok)
			assert.NotEmpty(t, reason, "a source that cannot be revived must say why")
		})
	}
}

// The kill is the fallback, not the default: it fires exactly when revival
// could not put the workload back.
func TestRecoverSourceKillsOnlyWhatCannotBeRevived(t *testing.T) {
	w, kills := recoveryController(t, []int{4242}, errors.New("process is failed"))

	w.recoverSourceAfterFailedDump(context.Background(), logr.Discard(), 0)

	assert.Equal(t, 1, *kills, "a source that cannot be revived must not be left hanging")
}

// The GPUs are handed back before CRIU's release is checked: a checkpointed
// process cannot run without them, so the order is load-bearing.
func TestReviveSourceHandsBackTheGPUsFirst(t *testing.T) {
	var order []string
	w := &NodeController{
		log:    logr.Discard(),
		config: &snapshottypes.AgentConfig{},
		filterCUDAProcessesFn: func(context.Context, []int, logr.Logger) []int {
			order = append(order, "filter")
			return []int{4242}
		},
		reviveGPUProcessesFn: func(context.Context, []int, string, logr.Logger) error {
			order = append(order, "revive")
			return errors.New("stop here")
		},
		sendSignalFn: func(logr.Logger, int, syscall.Signal, string) error { return nil },
	}

	_, ok := w.reviveSource(context.Background(), logr.Discard(), os.Getpid())

	require.False(t, ok)
	assert.Equal(t, []string{"filter", "revive"}, order)
}
