// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
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
