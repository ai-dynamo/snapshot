// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const captureLeaseSettleRequeue = 15 * time.Second

// ensureContentArtifactFinalizer guarantees the physical artifact remains tied
// to a Kubernetes identity until the operator has removed it from the PVC.
func (sr *PodSnapshotReconciler) ensureContentArtifactFinalizer(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent) (bool, error) {
	if controllerutil.ContainsFinalizer(content, podSnapshotContentArtifactFinalizer) {
		return false, nil
	}
	if !content.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf("PodSnapshotContent %q is deleting without artifact finalizer", content.Name)
	}
	controllerutil.AddFinalizer(content, podSnapshotContentArtifactFinalizer)
	if err := sr.Update(ctx, content); err != nil {
		return false, fmt.Errorf("add artifact finalizer to PodSnapshotContent %q: %w", content.Name, err)
	}
	return true, nil
}

func (sr *PodSnapshotReconciler) finalizeContentArtifacts(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(content, podSnapshotContentArtifactFinalizer) {
		return ctrl.Result{RequeueAfter: snapshotContentDeleteRequeue}, nil
	}
	contentUID := strings.TrimSpace(string(content.UID))
	if contentUID == "" {
		return ctrl.Result{}, fmt.Errorf("PodSnapshotContent %q has no UID", content.Name)
	}
	containers := content.Spec.Source.PodRef.Containers
	if len(containers) != 1 || strings.TrimSpace(containers[0]) == "" {
		return ctrl.Result{}, fmt.Errorf("PodSnapshotContent %q must reference exactly one container", content.Name)
	}

	leaseKey := client.ObjectKey{
		Namespace: content.Spec.PodSnapshotRef.Namespace,
		Name:      snapshotv1alpha1.CaptureLeaseName(contentUID, containers[0]),
	}
	lease := &coordinationv1.Lease{}
	if err := sr.Get(ctx, leaseKey, lease); err == nil {
		if !captureLeaseExpired(lease, time.Now()) {
			return ctrl.Result{RequeueAfter: snapshotContentDeleteRequeue}, nil
		}
		uid := lease.UID
		rv := lease.ResourceVersion
		if err := sr.Delete(ctx, lease, client.Preconditions{UID: &uid, ResourceVersion: &rv}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete expired capture Lease %s: %w", leaseKey.String(), err)
		}
		// Give a holder that stopped renewing enough time to observe the missing
		// Lease and cancel before the next cleanup pass removes its destination.
		return ctrl.Result{RequeueAfter: captureLeaseSettleRequeue}, nil
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get capture Lease %s: %w", leaseKey.String(), err)
	}

	artifactRoot, err := snapshotv1alpha1.ResolveArtifactRoot(sr.ArtifactBasePath, contentUID)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve artifact root for PodSnapshotContent %q: %w", content.Name, err)
	}
	removeAll := sr.removeAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	if err := removeAll(artifactRoot); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove artifact root %s: %w", artifactRoot, err)
	}

	controllerutil.RemoveFinalizer(content, podSnapshotContentArtifactFinalizer)
	if err := sr.Update(ctx, content); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove artifact finalizer from PodSnapshotContent %q: %w", content.Name, err)
	}
	return ctrl.Result{RequeueAfter: snapshotContentDeleteRequeue}, nil
}

func captureLeaseExpired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	last := lease.Spec.RenewTime
	if last == nil {
		last = lease.Spec.AcquireTime
	}
	if last == nil {
		return true
	}
	return now.After(last.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second))
}
