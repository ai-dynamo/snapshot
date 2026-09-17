// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/maintenance"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const podSnapshotContentArtifactCleanupControllerName = "podsnapshotcontent-artifact-cleanup"

// +kubebuilder:rbac:groups=nvidia.com,resources=podsnapshotcontents,verbs=get;list;watch;patch

func SetupSnapshotContentReconciler(mgr ctrl.Manager, queue maintenance.Enqueuer) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named(podSnapshotContentArtifactCleanupControllerName).
		For(&snapshotv1alpha1.PodSnapshotContent{}).
		Complete(reconcile.Func(func(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
			return reconcileSnapshotContent(ctx, mgr.GetClient(), queue, req)
		}))
}

func reconcileSnapshotContent(ctx context.Context, kubeClient client.Client, queue maintenance.Enqueuer, req ctrl.Request) (ctrl.Result, error) {
	content := &snapshotv1alpha1.PodSnapshotContent{}
	if err := kubeClient.Get(ctx, req.NamespacedName, content); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if content.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(content, maintenance.PodSnapshotContentArtifactCleanupFinalizer) {
			return ctrl.Result{}, nil
		}
		before := content.DeepCopy()
		controllerutil.AddFinalizer(content, maintenance.PodSnapshotContentArtifactCleanupFinalizer)
		return ctrl.Result{}, kubeClient.Patch(ctx, content, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	if !controllerutil.ContainsFinalizer(content, maintenance.PodSnapshotContentArtifactCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	queue.EnqueueDeleteContent(content.Namespace, content.Name, content.UID)
	return ctrl.Result{}, nil
}
