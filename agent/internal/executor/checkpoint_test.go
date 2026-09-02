// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/go-logr/logr"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type checkpointPathRuntime struct{}

func (checkpointPathRuntime) ResolveContainer(context.Context, string) (int, *specs.Spec, error) {
	return 0, nil, errors.New("stop after path preparation")
}

func (checkpointPathRuntime) ResolveContainerIDByPod(context.Context, string, string, string) (string, error) {
	return "", errors.New("not implemented")
}

func (checkpointPathRuntime) ResolveContainerByPod(context.Context, string, string, string) (int, *specs.Spec, error) {
	return 0, nil, errors.New("not implemented")
}

func (checkpointPathRuntime) Close() error { return nil }

func TestCheckpointPreparesContentArtifactParents(t *testing.T) {
	cfg := &types.AgentConfig{Storage: types.StorageSpec{BasePath: t.TempDir()}}
	finalDir, err := nsmount.ResolveArtifactPath(cfg.Storage.BasePath, "content-uid", "main")
	require.NoError(t, err)

	err = Checkpoint(context.Background(), checkpointPathRuntime{}, logr.Discard(), CheckpointRequest{
		ContentUID:    "content-uid",
		ContainerName: "main",
	}, cfg)
	require.ErrorContains(t, err, "stop after path preparation")
	assert.DirExists(t, filepath.Dir(finalDir))
	assert.DirExists(t, filepath.Join(cfg.Storage.BasePath, "artifacts", "content-uid", ".tmp"))
}

func TestValidatePOSIXCustomStorageTopology(t *testing.T) {
	for _, processCount := range []int{1, 2, 8} {
		if err := validatePOSIXCustomStorageTopology(processCount, 1); err != nil {
			t.Fatalf("validatePOSIXCustomStorageTopology(%d, 1) = %v", processCount, err)
		}
	}
	for _, topology := range []struct {
		processes int
		gpus      int
	}{{0, 1}, {1, 2}, {2, 2}} {
		err := validatePOSIXCustomStorageTopology(topology.processes, topology.gpus)
		if err == nil || !strings.Contains(err.Error(), "qualified only for one or more CUDA processes on one GPU") {
			t.Fatalf("validatePOSIXCustomStorageTopology(%d, %d) = %v, want qualification error",
				topology.processes, topology.gpus, err)
		}
	}
}

func TestReadCUDAProcessDetailsFailureIsPreMutation(t *testing.T) {
	_, err := readCUDAProcessDetailsForCheckpoint(t.TempDir(), []int{424242})
	if err == nil {
		t.Fatal("readCUDAProcessDetailsForCheckpoint() error = nil, want missing process error")
	}
	if !CheckpointFailedBeforeTargetMutation(err) {
		t.Fatalf("CheckpointFailedBeforeTargetMutation(%v) = false, want true", err)
	}
}

func TestTerminateCUDAProcessesAfterOperationFailurePropagatesCleanupError(t *testing.T) {
	var attempted []int
	err := terminateCUDAProcessesAfterOperationFailure(
		[]snapshotruntime.ProcessDetails{
			{OutermostPID: 41, StartTimeTicks: 100, Cgroup: "first"},
			{OutermostPID: 42, StartTimeTicks: 200, Cgroup: "second"},
		},
		"/test/proc",
		"checkpoint",
		logr.Discard(),
		func(procRoot string, process snapshotruntime.ProcessDetails) error {
			if procRoot != "/test/proc" {
				t.Fatalf("proc root = %q, want /test/proc", procRoot)
			}
			return nil
		},
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

func TestTerminateCUDAProcessesAfterOperationFailureRejectsChangedIdentity(t *testing.T) {
	var attempted []int
	err := terminateCUDAProcessesAfterOperationFailure(
		[]snapshotruntime.ProcessDetails{
			{OutermostPID: 41, StartTimeTicks: 100, Cgroup: "first"},
			{OutermostPID: 42, StartTimeTicks: 200, Cgroup: "second"},
		},
		"/test/proc",
		"checkpoint",
		logr.Discard(),
		func(_ string, process snapshotruntime.ProcessDetails) error {
			if process.OutermostPID == 41 {
				return errors.New("process identity changed")
			}
			return nil
		},
		func(_ logr.Logger, pid int, _ syscall.Signal, _ string) error {
			attempted = append(attempted, pid)
			return nil
		},
	)
	if len(attempted) != 1 || attempted[0] != 42 {
		t.Fatalf("attempted PIDs = %v, want [42]", attempted)
	}
	if err == nil || !strings.Contains(err.Error(), "process identity changed") {
		t.Fatalf("cleanup error = %v, want identity validation failure", err)
	}
}

func TestTerminateCUDAProcessesAfterRestoreFailureKillsEveryValidatedTarget(t *testing.T) {
	var attempted []int
	err := terminateCUDAProcessesAfterOperationFailure(
		[]snapshotruntime.ProcessDetails{
			{OutermostPID: 51, StartTimeTicks: 300, Cgroup: "first"},
			{OutermostPID: 52, StartTimeTicks: 400, Cgroup: "second"},
		},
		"/test/proc",
		"restore",
		logr.Discard(),
		func(string, snapshotruntime.ProcessDetails) error { return nil },
		func(_ logr.Logger, pid int, signal syscall.Signal, reason string) error {
			attempted = append(attempted, pid)
			if signal != syscall.SIGKILL || reason != "CUDA restore failed" {
				t.Fatalf("signal call = (%d, %q), want SIGKILL and restore reason", signal, reason)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("restore cleanup error = %v", err)
	}
	if len(attempted) != 2 || attempted[0] != 51 || attempted[1] != 52 {
		t.Fatalf("attempted PIDs = %v, want [51 52]", attempted)
	}
}
