// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"errors"
	"fmt"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/maintenance/backends"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// processDeleteContent is idempotent: a content that's already gone, already
// missing the finalizer, or recreated under the same name with a different
// UID is a completed no-op, not an error.
func (q *Queue) processDeleteContent(ctx context.Context, key WorkItemKey) error {
	content := &snapshotv1alpha1.PodSnapshotContent{}
	err := q.client.Get(ctx, apitypes.NamespacedName{Namespace: key.Namespace, Name: key.Name}, content)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get PodSnapshotContent %s/%s: %w", key.Namespace, key.Name, err)
	}
	if content.UID != key.UID {
		return nil
	}
	if content.DeletionTimestamp.IsZero() {
		return nil
	}
	if !controllerutil.ContainsFinalizer(content, PodSnapshotContentArtifactCleanupFinalizer) {
		return nil
	}

	backend, err := q.backend()
	if err != nil {
		return err
	}
	if err := backend.Delete(ctx, string(content.UID)); err != nil {
		if errors.Is(err, backends.ErrUnsafeArtifact) {
			q.recorder.Eventf(content, corev1.EventTypeWarning, ArtifactCleanupBlockedReason,
				"Artifact cleanup is blocked by an unsafe artifact root; remove it manually after verification: %v", err)
		}
		return err
	}

	before := content.DeepCopy()
	controllerutil.RemoveFinalizer(content, PodSnapshotContentArtifactCleanupFinalizer)
	return q.client.Patch(ctx, content, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// processSweep reschedules pending finalization and removes up to
// config.BatchSize confirmed orphans.
func (q *Queue) processSweep(ctx context.Context, logger logr.Logger) error {
	backend, err := q.backend()
	if err != nil {
		return err
	}

	// Enumerate before listing content so new artifacts aren't mistaken for orphans.
	candidates, enumerationErr := backend.Candidates(ctx, logger)
	scanResult, err := collectContentScanResult(ctx, q.apiReader, q.config.ListAttempts)
	if err != nil {
		return errors.Join(enumerationErr, err)
	}
	// Retry pending finalizers even when artifacts are absent or enumeration failed.
	for _, key := range scanResult.PendingDeletes {
		q.EnqueueDeleteContent(key.Namespace, key.Name, key.UID)
	}
	if enumerationErr != nil {
		return enumerationErr
	}
	var sweepErrors []error
	processed := 0
	for uid := range candidates {
		if _, protected := scanResult.ExistingUIDs[apitypes.UID(uid)]; protected {
			continue
		}
		if processed == q.config.BatchSize {
			break
		}
		processed++
		if err := backend.Delete(ctx, uid); err != nil {
			sweepErrors = append(sweepErrors, err)
			logger.Error(err, "Unable to reclaim orphan PodSnapshotContent artifact root", "content_uid", uid)
			continue
		}
		logger.Info("Reclaimed orphan PodSnapshotContent artifact root", "content_uid", uid)
	}
	return errors.Join(sweepErrors...)
}
