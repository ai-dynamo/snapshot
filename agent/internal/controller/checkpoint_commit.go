// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const checkpointCommitPendingReason = "CheckpointCommitPending"

func (w *NodeController) markCheckpointCommitPending(
	ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, cause error,
) error {
	patch := client.MergeFromWithOptions(content.DeepCopy(), client.MergeFromWithOptimisticLock{})
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type:               snapshotv1alpha1.PodSnapshotConditionReady,
		Status:             metav1.ConditionUnknown,
		Reason:             checkpointCommitPendingReason,
		Message:            cause.Error(),
		ObservedGeneration: content.Generation,
	})
	return w.client.Status().Patch(ctx, content, patch)
}

// A lost Commit reply can leave publication in progress; elapsed time alone does not prove failure.
func checkpointCommitPending(content *snapshotv1alpha1.PodSnapshotContent) bool {
	condition := meta.FindStatusCondition(content.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	return condition != nil && condition.Status == metav1.ConditionUnknown && condition.Reason == checkpointCommitPendingReason
}
