// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// SnapshotJobReconciler reconciles a SnapshotJob.
//
// This phase (source Job creation + Running) only handles the batch/v1 Job it
// creates: build/create it, adopt an existing one it already owns, classify a
// foreign object holding its deterministic name, and derive Running from
// job.status.ready. PodSnapshot creation (Captured), the completion gate
// (Completed/Failed beyond spec validation), and cleanup are added in later
// phases — this reconciler is not registered in main.go yet, so none of this
// runs in production until the feature is complete.
type SnapshotJobReconciler struct {
	client.Client
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotjobs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile drives a SnapshotJob's source Job into existence and derives Running
// from it. It is a thin orchestrator: each branch delegates to a helper that owns
// that path's detail.
func (r *SnapshotJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sj := &snapshotv1alpha1.SnapshotJob{}
	if err := r.Get(ctx, req.NamespacedName, sj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// No finalizer: the batch/v1 Job's ownerReference cascade reaps it when the
	// SnapshotJob is deleted, and the PodSnapshot (added in PR 4) must survive
	// deletion anyway, so there is nothing to clean up synchronously here.
	if !sj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	if snapshotv1alpha1.IsSnapshotJobTerminal(sj) {
		return ctrl.Result{}, nil
	}

	return r.reconcileJob(ctx, sj)
}

// reconcileJob is the Job phase of Reconcile: create the source Job when it does
// not exist yet, classify a foreign object holding its name, or observe an
// owned one for Running.
func (r *SnapshotJobReconciler) reconcileJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (ctrl.Result, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, job)
	switch {
	case apierrors.IsNotFound(err):
		return r.createJob(ctx, sj)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get source Job %q: %w", sj.Name, err)
	case !metav1.IsControlledBy(job, sj):
		// A foreign object already holds our deterministic Job name. Classified
		// here so we don't try to create or observe a Job that isn't ours; the
		// terminal Failed=True/JobNameConflict write is added by the completion
		// gate (a later phase), so this reconciler stops without taking an
		// action it can't yet finish recording.
		return ctrl.Result{}, nil
	}
	return r.observeJob(ctx, sj, job)
}

// createJob validates the spec, builds the source Job, and creates it with a
// controller ownerReference. Spec validation failures are terminal — recorded as
// Failed=True with an event, never returned as an error (which would retry
// forever against an object that can never become valid, since the spec is
// CEL-immutable).
func (r *SnapshotJobReconciler) createJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (ctrl.Result, error) {
	job, err := buildSourceJob(sj)
	if err != nil {
		return r.failSnapshotJob(ctx, sj, snapshotv1alpha1.ReasonInvalidSpec, err)
	}
	if err := controllerutil.SetControllerReference(sj, job, r.Scheme()); err != nil {
		return ctrl.Result{}, fmt.Errorf("set owner reference on source Job %q: %w", sj.Name, err)
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.adoptExistingJob(ctx, sj)
		}
		r.Recorder.Event(sj, corev1.EventTypeWarning, "SourceJobCreateFailed", err.Error())
		return ctrl.Result{}, fmt.Errorf("create source Job %q: %w", sj.Name, err)
	}

	if setCondition(sj, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse,
		snapshotv1alpha1.ReasonPodPending, "waiting for the source pod to become ready") {
		if err := r.Status().Update(ctx, sj); err != nil {
			return ctrl.Result{}, fmt.Errorf("update SnapshotJob status: %w", err)
		}
	}
	return ctrl.Result{}, nil
}

// adoptExistingJob handles Create returning AlreadyExists: a prior reconcile's
// Create already landed the Job server-side, but this reconcile's earlier Get
// missed it because the local watch cache hadn't caught up yet (a stale-cache
// race, not a real naming conflict). Re-Get and classify exactly like
// reconcileJob's Job-exists branch does — ours, observe it for Running; foreign,
// classify without acting (see reconcileJob's comment on JobNameConflict).
func (r *SnapshotJobReconciler) adoptExistingJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (ctrl.Result, error) {
	existing := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, existing); err != nil {
		return ctrl.Result{}, fmt.Errorf("get existing source Job %q after AlreadyExists: %w", sj.Name, err)
	}
	if !metav1.IsControlledBy(existing, sj) {
		return ctrl.Result{}, nil
	}
	return r.observeJob(ctx, sj, existing)
}

// observeJob derives Running from job.status.ready (GA in Kubernetes 1.29; beta-on
// since 1.24 behind the JobReadyPods feature gate) — the controller watches only
// the Job, not the pod, per the design's "SnapshotJob observes the Job, not the
// Pod, for failure status." startedAt is recorded once, the first time the pod is
// observed ready, and never rewritten afterward.
func (r *SnapshotJobReconciler) observeJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (ctrl.Result, error) {
	ready := job.Status.Ready != nil && *job.Status.Ready > 0

	var changed bool
	if ready {
		if sj.Status.StartedAt == nil {
			now := metav1.Now()
			sj.Status.StartedAt = &now
		}
		changed = setCondition(sj, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionTrue,
			snapshotv1alpha1.ReasonPodReady, "source pod is ready")
	} else {
		changed = setCondition(sj, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse,
			snapshotv1alpha1.ReasonPodPending, "waiting for the source pod to become ready")
	}

	if !changed {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Update(ctx, sj); err != nil {
		return ctrl.Result{}, fmt.Errorf("update SnapshotJob status: %w", err)
	}
	return ctrl.Result{}, nil
}

// failSnapshotJob records a terminal Failed=True condition and an event. It does
// not set completedAt itself — that belongs to the completion gate (a later
// phase), which owns the full Failed=True reason matrix; this phase only ever
// reaches it via InvalidSpec.
func (r *SnapshotJobReconciler) failSnapshotJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, reason string, cause error) (ctrl.Result, error) {
	r.Recorder.Event(sj, corev1.EventTypeWarning, reason, cause.Error())
	setCondition(sj, snapshotv1alpha1.SnapshotJobConditionFailed, metav1.ConditionTrue, reason, cause.Error())
	// completedAt must be set here: IsSnapshotJobTerminal short-circuits every
	// later reconcile once Failed=True is persisted, so this is the only chance
	// to record it for an InvalidSpec failure.
	if sj.Status.CompletedAt == nil {
		now := metav1.Now()
		sj.Status.CompletedAt = &now
	}
	if err := r.Status().Update(ctx, sj); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark SnapshotJob failed: %w", err)
	}
	return ctrl.Result{}, nil
}

// setCondition sets a status condition on the SnapshotJob and reports whether it changed.
func setCondition(sj *snapshotv1alpha1.SnapshotJob, condType string, status metav1.ConditionStatus, reason, message string) bool {
	return meta.SetStatusCondition(&sj.Status.Conditions, metav1.Condition{
		Type:    condType,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}

// SetupWithManager wires the controller: it owns the batch/v1 Job it creates (the
// Job carries a controller ownerRef, so this is the built-in mapping; Create is
// filtered since we just made it). PodSnapshot watching is added in a later phase.
func (r *SnapshotJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapshotv1alpha1.SnapshotJob{}).
		Owns(&batchv1.Job{}, builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return false },
			UpdateFunc:  func(event.UpdateEvent) bool { return true },
			DeleteFunc:  func(event.DeleteEvent) bool { return true },
			GenericFunc: func(event.GenericEvent) bool { return true },
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
