// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotjobs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=nvidia.com,resources=podsnapshots,verbs=create;get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=list
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// SnapshotJobReconciler reconciles a SnapshotJob.
//
// This phase (source Job creation, PodSnapshot creation, Running, Captured)
// handles the batch/v1 Job and PodSnapshot it creates: build/create the Job,
// adopt an existing one it already owns, classify a foreign object holding its
// deterministic name, derive Running from job.status.ready, create the
// PodSnapshot once the source pod exists, and mirror the PodSnapshot's
// Ready/Failed into Captured. The completion gate (Completed, and Failed beyond
// spec validation/PodSnapshot failure) and cleanup are added in a later phase —
// this reconciler is not registered in main.go yet, so none of this runs in
// production until the feature is complete.
type SnapshotJobReconciler struct {
	client.Client
	Recorder record.EventRecorder
}

// Reconcile drives a SnapshotJob's source Job into existence and derives Running
// from it. It is a thin orchestrator: each branch delegates to a helper that owns
// that path's detail.
func (r *SnapshotJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sj := &snapshotv1alpha1.SnapshotJob{}
	if err := r.Get(ctx, req.NamespacedName, sj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// No finalizer: GC cascade-deletes the owned Job; nothing else to clean up here.
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
		return r.failSnapshotJob(ctx, sj, snapshotv1alpha1.ReasonJobNameConflict,
			fmt.Errorf("an object not controlled by this SnapshotJob already holds the name %q", sj.Name))
	}
	return r.reconcilePodSnapshot(ctx, sj, job)
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
// reconcileJob's Job-exists branch does — ours, continue into the PodSnapshot
// phase; foreign, fail the SnapshotJob (see failSnapshotJob).
func (r *SnapshotJobReconciler) adoptExistingJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (ctrl.Result, error) {
	existing := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, existing); err != nil {
		return ctrl.Result{}, fmt.Errorf("get existing source Job %q after AlreadyExists: %w", sj.Name, err)
	}
	if !metav1.IsControlledBy(existing, sj) {
		return r.failSnapshotJob(ctx, sj, snapshotv1alpha1.ReasonJobNameConflict,
			fmt.Errorf("an object not controlled by this SnapshotJob already holds the name %q", sj.Name))
	}
	return r.reconcilePodSnapshot(ctx, sj, existing)
}

// reconcilePodSnapshot is the PodSnapshot phase of Reconcile, entered once the
// source Job is confirmed to exist and be owned: create the PodSnapshot as soon
// as the source pod exists (scheduled or not — PodSnapshotReconciler owns
// waiting for scheduling), or, once it exists, observe both the Job (Running)
// and the PodSnapshot (Captured).
func (r *SnapshotJobReconciler) reconcilePodSnapshot(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (ctrl.Result, error) {
	snap, err := r.findOwnedPodSnapshot(ctx, sj)
	switch {
	case apierrors.IsNotFound(err):
		return r.createPodSnapshotPhase(ctx, sj, job)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("find owned PodSnapshot: %w", err)
	}
	return r.observe(ctx, sj, job, snap)
}

// createPodSnapshotPhase looks up the source pod and, once it exists, creates
// the PodSnapshot. The pod may not exist yet even after the Job does (Job
// creation and pod creation are not atomic); in that case Running=False/PodPending
// is (re)set and this reconcile stops — the next Job status change (job.status.active
// 0→1 when the pod is assigned) re-enqueues, since the controller watches only
// the Job, not the pod.
func (r *SnapshotJobReconciler) createPodSnapshotPhase(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (ctrl.Result, error) {
	pod, err := findSourcePod(ctx, r.Client, job)
	if apierrors.IsNotFound(err) {
		if setCondition(sj, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse,
			snapshotv1alpha1.ReasonPodPending, "waiting for the source pod to be created") {
			if err := r.Status().Update(ctx, sj); err != nil {
				return ctrl.Result{}, fmt.Errorf("update SnapshotJob status: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("find source pod for Job %q: %w", job.Name, err)
	}

	snap, err := r.createPodSnapshot(ctx, sj, pod)
	if err != nil {
		if errors.Is(err, errPodSnapshotNameConflict) {
			return r.failSnapshotJob(ctx, sj, snapshotv1alpha1.ReasonPodSnapshotNameConflict, err)
		}
		return ctrl.Result{}, err
	}

	sj.Status.PodSnapshotName = snap.Name
	setCondition(sj, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionFalse,
		snapshotv1alpha1.ReasonCaptureInProgress, "waiting for the node agent to capture the checkpoint")
	if err := r.Status().Update(ctx, sj); err != nil {
		return ctrl.Result{}, fmt.Errorf("record PodSnapshot %q: %w", snap.Name, err)
	}
	return ctrl.Result{}, nil
}

// observe derives Running from job.status.ready and Captured from the
// PodSnapshot's own Ready/Failed conditions. Running: GA in Kubernetes 1.29
// (beta-on since 1.24 behind the JobReadyPods feature gate) — the controller
// watches only the Job, not the pod, per the design's "SnapshotJob observes the
// Job, not the Pod, for failure status." startedAt is recorded once, the first
// time the pod is observed ready, and never rewritten afterward. A capture
// failure (PodSnapshot Failed=True) is immediately terminal — Failed=True/
// CaptureFailed — independent of the Job's own completion, since it is fully
// determined by the PodSnapshot's own signal (unlike a Job failure after a
// successful capture, which needs the completion gate's Job-tracking, added in
// a later phase).
func (r *SnapshotJobReconciler) observe(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job, snap *snapshotv1alpha1.PodSnapshot) (ctrl.Result, error) {
	ready := job.Status.Ready != nil && *job.Status.Ready > 0

	var changed bool
	if ready {
		if sj.Status.StartedAt == nil {
			now := metav1.Now()
			sj.Status.StartedAt = &now
			changed = true
		}
		changed = setCondition(sj, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionTrue,
			snapshotv1alpha1.ReasonPodReady, "source pod is ready") || changed
	} else {
		changed = setCondition(sj, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse,
			snapshotv1alpha1.ReasonPodPending, "waiting for the source pod to become ready") || changed
	}

	switch {
	case snapshotv1alpha1.IsPodSnapshotFailed(snap):
		changed = setCondition(sj, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionFalse,
			snapshotv1alpha1.ReasonCaptureFailed, "node agent failed to capture the checkpoint") || changed
		changed = setCondition(sj, snapshotv1alpha1.SnapshotJobConditionFailed, metav1.ConditionTrue,
			snapshotv1alpha1.ReasonCaptureFailed, "node agent failed to capture the checkpoint") || changed
		if sj.Status.CompletedAt == nil {
			now := metav1.Now()
			sj.Status.CompletedAt = &now
		}
	case snapshotv1alpha1.IsPodSnapshotSucceeded(snap):
		changed = setCondition(sj, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionTrue,
			snapshotv1alpha1.ReasonCaptureCompleted, "CRIU dump of the target container is complete") || changed
	}

	if !changed {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Update(ctx, sj); err != nil {
		return ctrl.Result{}, fmt.Errorf("update SnapshotJob status: %w", err)
	}
	return ctrl.Result{}, nil
}

// failSnapshotJob records a terminal Failed=True condition, an event, and
// completedAt (the only chance to set it, since IsSnapshotJobTerminal
// short-circuits every later reconcile once Failed=True is persisted).
//
// It also backfills any of Running/Captured/Completed that was never set, to
// False — missing is not the same as known-False. Already-set conditions are
// left untouched. Running backfills with its own PodPending reason; Captured
// and Completed have no "never reached" reason of their own, so they reuse
// Failed's reason/cause.
func (r *SnapshotJobReconciler) failSnapshotJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, reason string, cause error) (ctrl.Result, error) {
	r.Recorder.Event(sj, corev1.EventTypeWarning, reason, cause.Error())
	if meta.FindStatusCondition(sj.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning) == nil {
		setCondition(sj, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse,
			snapshotv1alpha1.ReasonPodPending, "source pod was never observed ready before this SnapshotJob failed")
	}
	if meta.FindStatusCondition(sj.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured) == nil {
		setCondition(sj, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionFalse,
			reason, "capture was never started before this SnapshotJob failed: "+cause.Error())
	}
	if meta.FindStatusCondition(sj.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted) == nil {
		setCondition(sj, snapshotv1alpha1.SnapshotJobConditionCompleted, metav1.ConditionFalse,
			reason, "the SnapshotJob failed before completing: "+cause.Error())
	}
	setCondition(sj, snapshotv1alpha1.SnapshotJobConditionFailed, metav1.ConditionTrue, reason, cause.Error())
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

// SetupWithManager wires the controller: it owns the batch/v1 Job it creates
// (the Job carries a controller ownerRef, so this is the built-in mapping;
// Create is filtered since we just made it), and watches PodSnapshot via a
// label map function rather than Owns, since the produced PodSnapshot
// deliberately carries no ownerReference (§5.2 — artifacts must outlive the
// SnapshotJob). Create is filtered there too for the same reason: we just made
// it, so only later Update/Delete events carry new information.
func (r *SnapshotJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapshotv1alpha1.SnapshotJob{}).
		Owns(&batchv1.Job{}, builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return false },
			UpdateFunc:  func(event.UpdateEvent) bool { return true },
			DeleteFunc:  func(event.DeleteEvent) bool { return true },
			GenericFunc: func(event.GenericEvent) bool { return true },
		})).
		Watches(&snapshotv1alpha1.PodSnapshot{},
			handler.EnqueueRequestsFromMapFunc(mapPodSnapshotToSnapshotJob),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(event.CreateEvent) bool { return false },
				UpdateFunc:  func(event.UpdateEvent) bool { return true },
				DeleteFunc:  func(event.DeleteEvent) bool { return true },
				GenericFunc: func(event.GenericEvent) bool { return false },
			})).
		// MaxConcurrentReconciles: 1 is a deliberate choice for this phase (design
		// spec §5.1), not a scalability constraint discovered later — nothing in
		// this reconciler's logic requires serialization (deterministic per-object
		// names, no shared mutable state). Revisit once the controller is
		// registered in main.go and real concurrency needs are measured.
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
