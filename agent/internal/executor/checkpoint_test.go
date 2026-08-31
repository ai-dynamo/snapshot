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
	"github.com/ai-dynamo/snapshot/api/compat"
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

func (checkpointPathRuntime) ResolveContainerImageID(context.Context, string) (string, error) {
	return "", errors.New("not implemented")
}

func (checkpointPathRuntime) Close() error { return nil }

type checkpointImageRuntime struct {
	checkpointPathRuntime
}

func (checkpointImageRuntime) ResolveContainer(context.Context, string) (int, *specs.Spec, error) {
	return 1, &specs.Spec{}, nil
}

func (checkpointImageRuntime) ResolveContainerImageID(context.Context, string) (string, error) {
	return "", errors.New("runtime image unavailable")
}

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

func TestInspectContainerRequiresRuntimeImageID(t *testing.T) {
	_, _, err := inspectContainer(
		context.Background(),
		checkpointImageRuntime{},
		logr.Discard(),
		CheckpointRequest{ContainerID: "container-id"},
	)
	require.ErrorContains(t, err, "failed to resolve container image ID: runtime image unavailable")
}

func TestConfigureCheckpointRecordsRuntimeImageID(t *testing.T) {
	checkpointDir := t.TempDir()
	_, _, err := configureCheckpoint(
		logr.Discard(),
		&types.CheckpointContainerSnapshot{
			PID:        42,
			ImageID:    "sha256:runtime-content",
			RootFS:     "/",
			NetNSInode: 7,
		},
		CheckpointRequest{
			ContentUID:    "content-uid",
			ContainerID:   "container-id",
			ContainerName: "main",
			Pod: compat.Facts{
				Image:   "registry.example/workload:latest",
				ImageID: "sha256:kubelet-alias",
			},
		},
		&types.AgentConfig{},
		checkpointDir,
	)
	require.NoError(t, err)

	manifest, err := types.ReadManifest(checkpointDir)
	require.NoError(t, err)
	assert.Equal(t, "registry.example/workload:latest", manifest.K8s.Image)
	assert.Equal(t, "sha256:runtime-content", manifest.K8s.ImageID)
}
