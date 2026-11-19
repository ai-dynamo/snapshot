// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const nodeCacheTestContainer = "main"

func nodeCacheTarget() *restoreTarget {
	return &restoreTarget{SnapshotName: "snap", ContentUID: "content-uid", SourceContainerName: nodeCacheTestContainer}
}

// writeNodeCacheCopy places a checkpoint copy where an administrator would,
// recording manifestUID as the checkpoint it was taken for.
func writeNodeCacheCopy(t *testing.T, basePath, contentUID, manifestUID string) string {
	t.Helper()
	path, err := nsmount.ResolveArtifactPath(basePath, contentUID, nodeCacheTestContainer)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	if manifestUID != "" {
		require.NoError(t, types.WriteManifest(path, &types.CheckpointManifest{
			Artifact: types.ArtifactManifest{ContentUID: manifestUID, ContainerName: nodeCacheTestContainer},
		}))
	}
	return path
}

// nodeCacheController returns a controller with the node cache on, and content
// whose checkpoint is published in the snapshot store.
func nodeCacheController(t *testing.T) (*NodeController, *snapshotv1alpha1.PodSnapshotContent) {
	t.Helper()
	content := boundWorkOrder("content", "node-a", "x")
	content.Status.Storage = &snapshotv1alpha1.CheckpointStorageStatus{
		Artifacts: []snapshotv1alpha1.PublishedContainerArtifact{{ContainerName: nodeCacheTestContainer, ArtifactHandle: "h"}},
	}
	w := makeNodeController(t, &fakeCheckpointer{}, content)
	w.config.Storage.NodeCachePath = t.TempDir()
	return w, content
}

func TestResolveRestoreArtifactPrefersNodeCacheCopy(t *testing.T) {
	w, content := nodeCacheController(t)
	path := writeNodeCacheCopy(t, w.config.Storage.NodeCachePath, "content-uid", "content-uid")

	artifact, err := w.resolveRestoreArtifact("pod-key", nodeCacheTarget(), content)
	require.NoError(t, err)
	assert.True(t, artifact.NodeCache)
	assert.Equal(t, path, artifact.Path)
	assert.Nil(t, artifact.PublishedArtifact, "a node cache copy is read by path, not through the store")
	assert.Equal(t, "snap", artifact.SnapshotName)
	assert.Equal(t, "content-uid", artifact.ContentUID)
	assert.Equal(t, "Restoring from the node cache", restoreSourceMessage(artifact))
}

// A node cache problem never fails a restore: every miss falls back to the
// snapshot store, and the event says why the local copy was not used.
func TestResolveRestoreArtifactFallsBackToTheStore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, basePath string)
		miss    string
	}{
		{
			name:    "no copy",
			arrange: func(*testing.T, string) {},
			miss:    "this node holds no copy",
		},
		{
			name: "copy without a manifest",
			arrange: func(t *testing.T, basePath string) {
				writeNodeCacheCopy(t, basePath, "content-uid", "")
			},
			miss: "the copy on this node has no readable manifest",
		},
		{
			name: "copy of another checkpoint",
			arrange: func(t *testing.T, basePath string) {
				writeNodeCacheCopy(t, basePath, "content-uid", "other-uid")
			},
			miss: "the copy on this node belongs to another checkpoint",
		},
		{
			name: "copy behind a symlink",
			arrange: func(t *testing.T, basePath string) {
				elsewhere := writeNodeCacheCopy(t, t.TempDir(), "content-uid", "content-uid")
				path, err := nsmount.ResolveArtifactPath(basePath, "content-uid", nodeCacheTestContainer)
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.Symlink(elsewhere, path))
			},
			miss: "the copy on this node could not be read",
		},
		{
			name: "file where the copy belongs",
			arrange: func(t *testing.T, basePath string) {
				path, err := nsmount.ResolveArtifactPath(basePath, "content-uid", nodeCacheTestContainer)
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, nil, 0o600))
			},
			miss: "the copy on this node could not be read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, content := nodeCacheController(t)
			tc.arrange(t, w.config.Storage.NodeCachePath)

			artifact, err := w.resolveRestoreArtifact("pod-key", nodeCacheTarget(), content)
			require.NoError(t, err)
			assert.False(t, artifact.NodeCache)
			require.NotNil(t, artifact.PublishedArtifact, "the snapshot store must serve the restore")
			assert.Equal(t, tc.miss, artifact.NodeCacheMiss)
			assert.Equal(t, "Restoring from the snapshot store: "+tc.miss, restoreSourceMessage(artifact))
		})
	}
}

// With the node cache off nothing changes: no lookup, and no source event.
func TestResolveRestoreArtifactIgnoresNodeCacheWhenOff(t *testing.T) {
	w, content := nodeCacheController(t)
	writeNodeCacheCopy(t, w.config.Storage.NodeCachePath, "content-uid", "content-uid")
	w.config.Storage.NodeCachePath = ""

	artifact, err := w.resolveRestoreArtifact("pod-key", nodeCacheTarget(), content)
	require.NoError(t, err)
	assert.False(t, artifact.NodeCache)
	require.NotNil(t, artifact.PublishedArtifact)
	assert.Empty(t, artifact.NodeCacheMiss)
	assert.Empty(t, restoreSourceMessage(artifact))
}

// A store error still fails the restore; the node cache only adds a source.
func TestResolveRestoreArtifactKeepsStoreErrorsOnAMiss(t *testing.T) {
	w, content := nodeCacheController(t)
	content.Status.Storage = nil

	_, err := w.resolveRestoreArtifact("pod-key", nodeCacheTarget(), content)
	var pending *restorePendingError
	require.ErrorAs(t, err, &pending)
	assert.Equal(t, "ArtifactPending", pending.reason)
}

// A copy is already on the node's local disk, so it is read in place even
// where the snapshot store is configured to be staged.
func TestExecuteRestoreReadsANodeCacheCopyInPlace(t *testing.T) {
	for name, nodeCache := range map[string]bool{"snapshot store": false, "node cache": true} {
		t.Run(name, func(t *testing.T) {
			pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
			w := makeTestController(t, pod)
			w.config.PageBroker.RestoreMode = "staged"
			w.config.Storage.NodeCachePath = "/node-cache"
			var got executor.RestoreRequest
			w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
				got = req
				return executor.RestoreResult{}, nil
			}
			plan := &restorePlan{artifact: &restoreArtifact{NodeCache: nodeCache}}

			_, err := w.newRestoreOperation(pod, plan, "main", "container-id", time.Now()).executeRestore(context.Background())

			require.NoError(t, err)
			if nodeCache {
				assert.Equal(t, "direct", got.PageBrokerRestoreMode)
				assert.Equal(t, "/node-cache", got.BasePath)
			} else {
				assert.Equal(t, "staged", got.PageBrokerRestoreMode)
				assert.Equal(t, w.config.Storage.BasePath, got.BasePath)
			}
		})
	}
}
