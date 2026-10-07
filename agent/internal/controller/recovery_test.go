// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshottypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func publishedManifest(content *snapshotv1alpha1.PodSnapshotContent) *snapshottypes.CheckpointManifest {
	manifest := recordedManifest()
	manifest.Artifact = snapshottypes.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"}
	return manifest
}

func TestBoundRecoveryPromotesAnExistingPublicationWithItsSource(t *testing.T) {
	content := boundWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	pod.Status.Phase = corev1.PodFailed
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	var probed *pagebroker.PublishedArtifact
	w.fetchManifestFn = func(_ context.Context, artifact *pagebroker.PublishedArtifact) (*snapshottypes.CheckpointManifest, error) {
		probed = artifact
		return publishedManifest(content), nil
	}

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	assert.False(t, fc.wasCalled(), "a committed publication is recovered, not re-dumped")
	require.NotNil(t, probed)
	assert.Equal(t, testStoreID, probed.GetStoreId())
	assert.Equal(t, "artifacts/"+string(content.UID)+"/containers/main", probed.GetArtifactHandle())
	got := getContent(t, w, content.Name)
	assert.NotNil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	require.NotNil(t, got.Status.Storage)
	require.Len(t, got.Status.Storage.Artifacts, 1)
	assert.Equal(t, probed.GetArtifactHandle(), got.Status.Storage.Artifacts[0].ArtifactHandle)
	assert.Equal(t, recordedSource(), got.Status.Source)
}

func TestBoundRecoveryWithoutAPublicationFailsAGonePod(t *testing.T) {
	content := boundWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content)
	w.fetchManifestFn = func(context.Context, *pagebroker.PublishedArtifact) (*snapshottypes.CheckpointManifest, error) {
		return nil, pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_NOT_FOUND, "publication not found")
	}

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	got := getContent(t, w, content.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "SourcePodNotFound", cond.Reason)
}

func TestBoundRecoveryRetriesWhenTheBrokerIsUnreachable(t *testing.T) {
	content := boundWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	w := makeNodeController(t, &fakeCheckpointer{}, content)
	w.fetchManifestFn = func(context.Context, *pagebroker.PublishedArtifact) (*snapshottypes.CheckpointManifest, error) {
		return nil, errors.New("dial PageBroker: connection refused")
	}

	err := w.reconcileCapture(context.Background(), content.Name)

	require.ErrorContains(t, err, "probe published artifact")
	got := getContent(t, w, content.Name)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed),
		"an unanswered probe is not proof of absence")
}

func TestBoundRecoveryRejectsAManifestForAnotherArtifact(t *testing.T) {
	content := boundWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	w := makeNodeController(t, &fakeCheckpointer{}, content)
	w.fetchManifestFn = func(context.Context, *pagebroker.PublishedArtifact) (*snapshottypes.CheckpointManifest, error) {
		manifest := recordedManifest()
		manifest.Artifact = snapshottypes.ArtifactManifest{ContentUID: "someone-else", ContainerName: "main"}
		return manifest, nil
	}

	_, _, err := w.committedPublication(context.Background(), content, "main")
	require.ErrorContains(t, err, "names someone-else/main")
}

func TestMarkCheckpointReadyRecordsTheSourceOfABoundCapture(t *testing.T) {
	content := boundWorkOrder("content", "node-a", "x")
	content.ResourceVersion = "1"
	w := makeNodeController(t, &fakeCheckpointer{}, content)
	w.fetchManifestFn = func(context.Context, *pagebroker.PublishedArtifact) (*snapshottypes.CheckpointManifest, error) {
		return publishedManifest(content), nil
	}
	published := pagebroker.FilesystemArtifact(testStoreID, string(content.UID), "main")

	require.NoError(t, w.markCheckpointReady(context.Background(), content, "", "main", published))

	got := getContent(t, w, content.Name)
	assert.Equal(t, recordedSource(), got.Status.Source)
	require.NotNil(t, got.Status.Storage)
	assert.Equal(t, published.GetArtifactHandle(), got.Status.Storage.Artifacts[0].ArtifactHandle)
}
