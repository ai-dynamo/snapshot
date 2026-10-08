// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const testStoreID = "store-v1-0000000000000000000000000000000000000000000000000000000000000001"

func boundWorkOrder(name, node, suffix string) *snapshotv1alpha1.PodSnapshotContent {
	content := makeWorkOrder(name, node, suffix)
	content.Spec.Storage = &snapshotv1alpha1.CheckpointStorageBinding{StoreID: testStoreID}
	return content
}

func TestResolveBoundRestoreArtifactFindsPublishedContainer(t *testing.T) {
	content := boundWorkOrder("content", "node-a", "x")
	content.Status.Storage = &snapshotv1alpha1.CheckpointStorageStatus{
		Artifacts: []snapshotv1alpha1.PublishedContainerArtifact{
			{ContainerName: "other", ArtifactHandle: "handle-other", ArtifactFormatVersion: "v1"},
			{ContainerName: "main", ArtifactHandle: "artifacts/content/containers/main", ArtifactFormatVersion: "snapshot.pagebroker/v1"},
		},
	}
	w := &NodeController{}
	target := &restoreTarget{SnapshotName: "snap", ContentUID: "content-uid", SourceContainerName: "main"}

	artifact, err := w.resolveBoundRestoreArtifact(target, content)
	require.NoError(t, err)
	require.NotNil(t, artifact.PublishedArtifact)
	assert.Equal(t, testStoreID, artifact.PublishedArtifact.StoreId)
	assert.Equal(t, "artifacts/content/containers/main", artifact.PublishedArtifact.ArtifactHandle)
	assert.Equal(t, "snapshot.pagebroker/v1", artifact.PublishedArtifact.ArtifactFormatVersion)
	assert.Equal(t, "content-uid", artifact.ContentUID)
	assert.Equal(t, "main", artifact.SourceContainerName)
	assert.Equal(t, "snap", artifact.SnapshotName)
}

func TestResolveBoundRestoreArtifactPendingWhenNotYetPublished(t *testing.T) {
	withOtherContainer := boundWorkOrder("content", "node-a", "x")
	withOtherContainer.Status.Storage = &snapshotv1alpha1.CheckpointStorageStatus{
		Artifacts: []snapshotv1alpha1.PublishedContainerArtifact{
			{ContainerName: "other", ArtifactHandle: "h", ArtifactFormatVersion: "v1"},
		},
	}
	for _, tc := range []struct {
		name    string
		content *snapshotv1alpha1.PodSnapshotContent
	}{
		{"no status at all", boundWorkOrder("content", "node-a", "x")},
		{"status for a different container", withOtherContainer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &NodeController{}
			target := &restoreTarget{SnapshotName: "snap", ContentUID: "content-uid", SourceContainerName: "main"}

			_, err := w.resolveBoundRestoreArtifact(target, tc.content)
			var pending *restorePendingError
			require.ErrorAs(t, err, &pending)
			assert.Equal(t, "ArtifactPending", pending.reason)
		})
	}
}

func TestResolveRestoreArtifactRoutesBoundContentToStatusLookup(t *testing.T) {
	content := boundWorkOrder("content", "node-a", "x")
	content.Status.Storage = &snapshotv1alpha1.CheckpointStorageStatus{
		Artifacts: []snapshotv1alpha1.PublishedContainerArtifact{
			{ContainerName: "main", ArtifactHandle: "h", ArtifactFormatVersion: "v1"},
		},
	}
	w := makeNodeController(t, &fakeCheckpointer{}, content)
	target := &restoreTarget{SnapshotName: "snap", ContentUID: "content-uid", SourceContainerName: "main"}

	artifact, err := w.resolveRestoreArtifact("pod-key", target, content)
	require.NoError(t, err)
	require.NotNil(t, artifact.PublishedArtifact)
	assert.Empty(t, artifact.Path, "bound content must not resolve a legacy filesystem path")
}

func TestSetSnapshotContentSucceededPersistsPublishedArtifact(t *testing.T) {
	content := boundWorkOrder("content", "node-a", "x")
	content.ResourceVersion = "1"
	w := makeNodeController(t, &fakeCheckpointer{}, content)

	published := &pagebroker.PublishedArtifact{
		StoreId:               testStoreID,
		ArtifactHandle:        "artifacts/content/containers/main",
		ArtifactFormatVersion: "snapshot.pagebroker/v1",
	}
	require.NoError(t, w.setSnapshotContentSucceeded(context.Background(), content, nil, "main", published))

	current := getContent(t, w, content.Name)
	require.NotNil(t, current.Status.Storage)
	require.Len(t, current.Status.Storage.Artifacts, 1)
	assert.Equal(t, "main", current.Status.Storage.Artifacts[0].ContainerName)
	assert.Equal(t, "artifacts/content/containers/main", current.Status.Storage.Artifacts[0].ArtifactHandle)
	assert.Equal(t, "snapshot.pagebroker/v1", current.Status.Storage.Artifacts[0].ArtifactFormatVersion)
}

func TestSetSnapshotContentSucceededReplacesExistingArtifactForSameContainer(t *testing.T) {
	content := boundWorkOrder("content", "node-a", "x")
	content.ResourceVersion = "1"
	content.Status.Storage = &snapshotv1alpha1.CheckpointStorageStatus{
		Artifacts: []snapshotv1alpha1.PublishedContainerArtifact{
			{ContainerName: "main", ArtifactHandle: "stale-handle", ArtifactFormatVersion: "v0"},
		},
	}
	w := makeNodeController(t, &fakeCheckpointer{}, content)

	published := &pagebroker.PublishedArtifact{
		StoreId:               testStoreID,
		ArtifactHandle:        "artifacts/content/containers/main/v2",
		ArtifactFormatVersion: "snapshot.pagebroker/v1",
	}
	require.NoError(t, w.setSnapshotContentSucceeded(context.Background(), content, nil, "main", published))

	current := getContent(t, w, content.Name)
	require.Len(t, current.Status.Storage.Artifacts, 1, "must replace the stale entry for the same container, not append a duplicate")
	assert.Equal(t, "artifacts/content/containers/main/v2", current.Status.Storage.Artifacts[0].ArtifactHandle)
}

func TestMarkCheckpointReadySkipsFilesystemSourceReadWhenBound(t *testing.T) {
	content := boundWorkOrder("content", "node-a", "x")
	content.ResourceVersion = "1"
	w := makeNodeController(t, &fakeCheckpointer{}, content)

	published := &pagebroker.PublishedArtifact{
		StoreId:               testStoreID,
		ArtifactHandle:        "artifacts/content/containers/main",
		ArtifactFormatVersion: "snapshot.pagebroker/v1",
	}
	// t.TempDir() has no manifest; bound content must not try to read one.
	err := w.markCheckpointReady(context.Background(), content, t.TempDir(), "main", published)
	require.NoError(t, err)

	current := getContent(t, w, content.Name)
	assert.Nil(t, current.Status.Source, "bound checkpoints do not read a filesystem manifest for Source")
	require.NotNil(t, current.Status.Storage)
	require.Len(t, current.Status.Storage.Artifacts, 1)
	assert.Equal(t, "artifacts/content/containers/main", current.Status.Storage.Artifacts[0].ArtifactHandle)
	assert.NotNil(t, meta.FindStatusCondition(current.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
}
