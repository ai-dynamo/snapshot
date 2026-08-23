// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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
// Resource helpers create, find, and classify the Job, source Pod, and
// PodSnapshot without mutating SnapshotJob status. Reconcile then derives the
// complete status from those observations and persists it once. This makes
// every resource-create/status-write race recoverable on the next reconcile.
type SnapshotJobReconciler struct {
	client.Client
	APIReader client.Reader
	Recorder  record.EventRecorder
}

const sourcePodRequeueBackstop = 2 * time.Second

type snapshotJobFailure struct {
	reason string
	cause  error
}

type snapshotJobObservation struct {
	job              *batchv1.Job
	podSnapshot      *snapshotv1alpha1.PodSnapshot
	sourcePodMissing bool
	failure          *snapshotJobFailure
}

// Reconcile first drives child resources toward the desired state, then derives
// and patches SnapshotJob status once from the resulting observation.
func (r *SnapshotJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sj := &snapshotv1alpha1.SnapshotJob{}
	if err := r.Get(ctx, req.NamespacedName, sj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sj.GetDeletionTimestamp().IsZero() || snapshotv1alpha1.IsSnapshotJobTerminal(sj) {
		return ctrl.Result{}, nil
	}

	observed, result, err := r.reconcileResources(ctx, sj)
	if err != nil {
		return result, err
	}
	if observed.failure != nil {
		r.Recorder.Event(sj, corev1.EventTypeWarning, observed.failure.reason, observed.failure.cause.Error())
	}

	desiredStatus := deriveSnapshotJobStatus(sj, observed)
	if err := r.patchSnapshotJobStatus(ctx, sj, desiredStatus); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

// reconcileResources observes an existing source Job or builds and creates it
// when absent, then reconciles its PodSnapshot. Retryable API failures are
// returned as errors; invalid input on the create path and deterministic-name
// conflicts are typed observations.
func (r *SnapshotJobReconciler) reconcileResources(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (snapshotJobObservation, ctrl.Result, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, job)
	switch {
	case apierrors.IsNotFound(err):
		desiredJob, buildErr := buildSourceJob(sj)
		if buildErr != nil {
			return terminalObservation(snapshotv1alpha1.ReasonInvalidSpec, buildErr), ctrl.Result{}, nil
		}
		return r.createSourceJob(ctx, sj, desiredJob)
	case err != nil:
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("get source Job %q: %w", sj.Name, err)
	case !metav1.IsControlledBy(job, sj):
		return terminalObservation(snapshotv1alpha1.ReasonJobNameConflict,
			fmt.Errorf("an object not controlled by this SnapshotJob already holds the name %q", sj.Name)), ctrl.Result{}, nil
	default:
		return r.reconcilePodSnapshotResources(ctx, sj, job)
	}
}

// createSourceJob sets ownership and creates the desired source Job. An
// AlreadyExists response can be a cache race, so it is re-read and classified;
// other API failures remain retryable.
func (r *SnapshotJobReconciler) createSourceJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, desiredJob *batchv1.Job) (snapshotJobObservation, ctrl.Result, error) {
	if err := controllerutil.SetControllerReference(sj, desiredJob, r.Scheme()); err != nil {
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("set owner reference on source Job %q: %w", sj.Name, err)
	}
	if err := r.Create(ctx, desiredJob); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.observeExistingSourceJob(ctx, sj)
		}
		r.Recorder.Event(sj, corev1.EventTypeWarning, "SourceJobCreateFailed", err.Error())
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("create source Job %q: %w", sj.Name, err)
	}
	return snapshotJobObservation{job: desiredJob}, ctrl.Result{}, nil
}

// observeExistingSourceJob classifies the object returned by a create/Get cache
// race and continues resource reconciliation when it belongs to this SnapshotJob.
func (r *SnapshotJobReconciler) observeExistingSourceJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (snapshotJobObservation, ctrl.Result, error) {
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, job); err != nil {
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("get existing source Job %q after AlreadyExists: %w", sj.Name, err)
	}
	if !metav1.IsControlledBy(job, sj) {
		return terminalObservation(snapshotv1alpha1.ReasonJobNameConflict,
			fmt.Errorf("an object not controlled by this SnapshotJob already holds the name %q", sj.Name)), ctrl.Result{}, nil
	}
	return r.reconcilePodSnapshotResources(ctx, sj, job)
}

// reconcilePodSnapshotResources observes an existing PodSnapshot or creates it
// once exactly one controlled source Pod exists.
func (r *SnapshotJobReconciler) reconcilePodSnapshotResources(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (snapshotJobObservation, ctrl.Result, error) {
	observed := snapshotJobObservation{job: job}
	snap, err := r.findOwnedPodSnapshot(ctx, sj)
	switch {
	case errors.Is(err, errPodSnapshotNameConflict):
		observed.failure = &snapshotJobFailure{reason: snapshotv1alpha1.ReasonPodSnapshotNameConflict, cause: err}
		return observed, ctrl.Result{}, nil
	case apierrors.IsNotFound(err):
		return r.createPodSnapshotForSourceJob(ctx, sj, job)
	case err != nil:
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("find owned PodSnapshot: %w", err)
	}

	observed.podSnapshot = snap
	return observed, ctrl.Result{}, nil
}

// createPodSnapshotForSourceJob waits for the source Pod, then creates or
// classifies the deterministic PodSnapshot. A missing Pod gets a bounded
// backstop requeue; API failures retry and name conflicts are terminal.
func (r *SnapshotJobReconciler) createPodSnapshotForSourceJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (snapshotJobObservation, ctrl.Result, error) {
	observed := snapshotJobObservation{job: job}
	pod, err := findSourcePod(ctx, r.sourcePodReader(), job)
	if apierrors.IsNotFound(err) {
		observed.sourcePodMissing = true
		return observed, ctrl.Result{RequeueAfter: sourcePodRequeueBackstop}, nil
	}
	if err != nil {
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("find source pod for Job %q: %w", job.Name, err)
	}

	snap, err := r.createPodSnapshot(ctx, sj, pod)
	if errors.Is(err, errPodSnapshotNameConflict) {
		observed.failure = &snapshotJobFailure{reason: snapshotv1alpha1.ReasonPodSnapshotNameConflict, cause: err}
		return observed, ctrl.Result{}, nil
	}
	if err != nil {
		return snapshotJobObservation{}, ctrl.Result{}, err
	}

	observed.podSnapshot = snap
	return observed, ctrl.Result{}, nil
}

func (r *SnapshotJobReconciler) sourcePodReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	// Tests construct the reconciler directly without SetupWithManager.
	return r.Client
}

func terminalObservation(reason string, cause error) snapshotJobObservation {
	return snapshotJobObservation{failure: &snapshotJobFailure{reason: reason, cause: cause}}
}

// deriveSnapshotJobStatus is a pure derivation over current status and observed
// resources. Existing timestamps are monotonic; conditions and references are
// reconstructed whenever their source resource is observed.
func deriveSnapshotJobStatus(sj *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation) snapshotv1alpha1.SnapshotJobStatus {
	next := sj.DeepCopy()
	deriveRunningStatus(next, observed)
	failure := deriveCapturedStatus(next, observed)
	deriveFailureStatus(next, failure)
	return next.Status
}

func deriveRunningStatus(next *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation) {
	if observed.job != nil {
		ready := observed.job.Status.Ready != nil && *observed.job.Status.Ready > 0
		if ready {
			if next.Status.StartedAt == nil {
				now := metav1.Now()
				next.Status.StartedAt = &now
			}
			setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionTrue,
				snapshotv1alpha1.ReasonPodReady, "source pod is ready")
		} else {
			message := "waiting for the source pod to become ready"
			if observed.sourcePodMissing {
				message = "waiting for the source Job to create a pod"
			}
			setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse,
				snapshotv1alpha1.ReasonPodPending, message)
		}
	}
}

func deriveCapturedStatus(next *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation) *snapshotJobFailure {
	failure := observed.failure
	if observed.podSnapshot != nil {
		next.Status.PodSnapshotName = observed.podSnapshot.Name
		switch {
		case snapshotv1alpha1.IsPodSnapshotFailed(observed.podSnapshot):
			failure = &snapshotJobFailure{reason: snapshotv1alpha1.ReasonCaptureFailed,
				cause: errors.New("node agent failed to capture the checkpoint")}
		case snapshotv1alpha1.IsPodSnapshotSucceeded(observed.podSnapshot):
			setCondition(next, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionTrue,
				snapshotv1alpha1.ReasonCaptureCompleted, "CRIU dump of the target container is complete")
		default:
			setCondition(next, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionFalse,
				snapshotv1alpha1.ReasonCaptureInProgress, "waiting for the node agent to capture the checkpoint")
		}
	}
	return failure
}

func deriveFailureStatus(next *snapshotv1alpha1.SnapshotJob, failure *snapshotJobFailure) {
	if failure == nil {
		return
	}
	if meta.FindStatusCondition(next.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning) == nil {
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse,
			snapshotv1alpha1.ReasonPodPending, "source pod was never observed ready before this SnapshotJob failed")
	}
	setCondition(next, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionFalse,
		failure.reason, "capture did not complete: "+failure.cause.Error())
	setCondition(next, snapshotv1alpha1.SnapshotJobConditionCompleted, metav1.ConditionFalse,
		failure.reason, "the SnapshotJob failed before completing: "+failure.cause.Error())
	setCondition(next, snapshotv1alpha1.SnapshotJobConditionFailed, metav1.ConditionTrue,
		failure.reason, failure.cause.Error())
	if next.Status.CompletedAt == nil {
		now := metav1.Now()
		next.Status.CompletedAt = &now
	}
}

func (r *SnapshotJobReconciler) patchSnapshotJobStatus(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, desired snapshotv1alpha1.SnapshotJobStatus) error {
	if apiequality.Semantic.DeepEqual(sj.Status, desired) {
		return nil
	}
	updated := sj.DeepCopy()
	updated.Status = desired
	if err := r.Status().Patch(ctx, updated, client.MergeFrom(sj)); err != nil {
		return fmt.Errorf("patch SnapshotJob status: %w", err)
	}
	return nil
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
// and watches PodSnapshot via a label map function because capture artifacts
// deliberately carry no ownerReference and must outlive the SnapshotJob.
func (r *SnapshotJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.APIReader = mgr.GetAPIReader()
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
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
