// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
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

func TestCheckpointPageBrokerPrepareFailureDoesNotMutate(t *testing.T) {
	cfg := &types.AgentConfig{
		Storage:    types.StorageSpec{BasePath: t.TempDir()},
		PageBroker: types.PageBrokerSpec{ControlSocketPath: t.TempDir() + "/pagebroker.sock"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	err := Checkpoint(ctx, checkpointPathRuntime{}, logr.Discard(), CheckpointRequest{
		ContentUID:    "content-uid",
		ContainerName: "main",
	}, cfg)
	require.ErrorContains(t, err, "prepare PageBroker checkpoint")
	assert.NotContains(t, err.Error(), "abort PageBroker checkpoint")
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.False(t, CheckpointNeedsSourceKill(err))
	assert.NoDirExists(t, filepath.Join(cfg.Storage.BasePath, "artifacts"))
}

func TestCheckpointNeedsSourceKill(t *testing.T) {
	assert.True(t, CheckpointNeedsSourceKill(checkpointNeedsSourceKill(errors.New("capture failed"))))
	assert.False(t, CheckpointNeedsSourceKill(errors.New("prepare failed")))
	assert.False(t, CheckpointNeedsSourceKill(fmt.Errorf("commit PageBroker checkpoint: %w", errors.New("failed"))))
}
