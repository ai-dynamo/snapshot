// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
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
