// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"fmt"

	"github.com/ai-dynamo/snapshot/api/storage/coordination"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RepairPublication reconciles a container's recorded descriptor against
// caller-supplied evidence (discovery is backend-specific, not this
// package's job), refusing conflicting or missing evidence. Uses the same
// lock as delete/sweep.
func (q *Queue) RepairPublication(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, containerName string, found []coordination.Evidence) error {
	if content.Spec.Storage == nil {
		return fmt.Errorf("content %s has no storage binding to repair against", content.Name)
	}
	if _, err := q.backendForContent(content); err != nil {
		return err
	}
	storeID := content.Spec.Storage.StoreID
	commitID, err := coordination.ValidateBinding(storeID, string(content.UID), containerName)
	if err != nil {
		return err
	}

	unlock := q.locker.Lock(storeKey(content))
	defer unlock()

	evidence, err := coordination.MatchEvidence(commitID, found)
	if err != nil {
		return fmt.Errorf("repair publication for container %q: %w", containerName, err)
	}

	before := content.DeepCopy()
	if content.Status.Storage == nil {
		content.Status.Storage = &snapshotv1alpha1.CheckpointStorageStatus{}
	}
	repaired := snapshotv1alpha1.PublishedContainerArtifact{
		ContainerName:         containerName,
		ArtifactHandle:        evidence.ArtifactHandle,
		ArtifactFormatVersion: evidence.FormatVersion,
	}
	replaced := false
	for i, artifact := range content.Status.Storage.Artifacts {
		if artifact.ContainerName == containerName {
			content.Status.Storage.Artifacts[i] = repaired
			replaced = true
			break
		}
	}
	if !replaced {
		content.Status.Storage.Artifacts = append(content.Status.Storage.Artifacts, repaired)
	}
	return q.client.Status().Patch(ctx, content, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
