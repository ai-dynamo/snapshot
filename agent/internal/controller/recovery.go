// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// committedPublication asks PageBroker whether the deterministic publication
// for a bound capture already exists, so a capture whose Commit reply or Ready
// write was lost is recovered from the store rather than re-dumped or failed.
// Returns nil when the store has no such publication.
func (w *NodeController) committedPublication(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	containerName string,
) (*pagebroker.PublishedArtifact, *types.CheckpointManifest, error) {
	if w.fetchManifestFn == nil {
		return nil, nil, nil
	}
	artifact := pagebroker.FilesystemArtifact(content.Spec.Storage.StoreID, string(content.UID), containerName)
	manifest, err := w.fetchManifestFn(ctx, artifact)
	if err != nil {
		if executor.IsArtifactNotFound(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("probe published artifact for %s/%s: %w", content.Name, containerName, err)
	}
	if manifest.Artifact.ContentUID != string(content.UID) || manifest.Artifact.ContainerName != containerName {
		return nil, nil, fmt.Errorf("published artifact %s names %s/%s, want %s/%s",
			artifact.GetArtifactHandle(), manifest.Artifact.ContentUID, manifest.Artifact.ContainerName, content.UID, containerName)
	}
	return artifact, manifest, nil
}
