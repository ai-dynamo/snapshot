// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ai-dynamo/snapshot/agent/pkg/artifact"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	PodSnapshotContentArtifactCleanupFinalizer      = "nvidia.com/podsnapshotcontent-artifact-cleanup"
	podSnapshotContentArtifactCleanupControllerName = "podsnapshotcontent-artifact-cleanup"
	podSnapshotContentArtifactCleanupBlockedReason  = "ArtifactCleanupBlocked"
)

var errUnsafeArtifactRoot = errors.New("artifact root is not an ordinary directory")

// +kubebuilder:rbac:groups=nvidia.com,resources=podsnapshotcontents,verbs=get;list;watch;patch

func SetupSnapshotContentReconciler(mgr ctrl.Manager, basePath string) error {
	recorder := mgr.GetEventRecorderFor(podSnapshotContentArtifactCleanupControllerName)
	return ctrl.NewControllerManagedBy(mgr).
		Named(podSnapshotContentArtifactCleanupControllerName).
		For(&snapshotv1alpha1.PodSnapshotContent{}).
		Complete(reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
			return reconcileSnapshotContent(ctx, mgr.GetClient(), recorder, basePath, req)
		}))
}

func reconcileSnapshotContent(ctx context.Context, kubeClient client.Client, recorder record.EventRecorder, basePath string, req ctrl.Request) (ctrl.Result, error) {
	content := &snapshotv1alpha1.PodSnapshotContent{}
	if err := kubeClient.Get(ctx, req.NamespacedName, content); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if content.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(content, PodSnapshotContentArtifactCleanupFinalizer) {
			return ctrl.Result{}, nil
		}
		before := content.DeepCopy()
		controllerutil.AddFinalizer(content, PodSnapshotContentArtifactCleanupFinalizer)
		return ctrl.Result{}, kubeClient.Patch(ctx, content, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if !controllerutil.ContainsFinalizer(content, PodSnapshotContentArtifactCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := removeArtifactRoot(basePath, string(content.UID)); err != nil {
		if errors.Is(err, errUnsafeArtifactRoot) {
			recorder.Eventf(content, corev1.EventTypeWarning, podSnapshotContentArtifactCleanupBlockedReason,
				"Artifact cleanup is blocked by an unsafe artifact root; remove it manually after verification: %v", err)
		}
		return ctrl.Result{}, err
	}

	before := content.DeepCopy()
	controllerutil.RemoveFinalizer(content, PodSnapshotContentArtifactCleanupFinalizer)
	return ctrl.Result{}, kubeClient.Patch(ctx, content, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func removeArtifactRoot(basePath, contentUID string) error {
	artifactsRoot, err := artifact.ResolveRoot(basePath)
	if err != nil {
		return err
	}
	if err := artifact.ValidateDirectory(artifactsRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: %w", errUnsafeArtifactRoot, err)
	}
	root, err := artifact.ResolveContentRoot(basePath, contentUID)
	if err != nil {
		return err
	}
	if err := artifact.ValidateDirectory(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", errUnsafeArtifactRoot, err)
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove artifact root %q: %w", root, err)
	}
	return nil
}
