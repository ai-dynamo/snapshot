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
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;get;list;watch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=list
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// SnapshotJobReconciler reconciles a SnapshotJob.
//
// Resource helpers create, find, and classify the Job, source Pod, and
// PodSnapshot without mutating SnapshotJob status. Reconcile then derives the
// complete status from those observations and persists it once. Capture success
// is terminal and triggers source Job cleanup; failures preserve the Job for
// debugging.
type SnapshotJobReconciler struct {
	client.Client
	APIReader client.Reader
	Recorder  record.EventRecorder
}

const (
	sourcePodRequeueBackstop     = 2 * time.Second
	captureResultRequeueBackstop = 2 * time.Second
)

type snapshotJobFailure struct {
	reason string
	cause  error
}

type snapshotJobObservation struct {
	job                     *batchv1.Job
	podSnapshot             *snapshotv1alpha1.PodSnapshot
	sourcePodMissing        bool
	waitingForCaptureResult bool
	failure                 *snapshotJobFailure
}

// Reconcile first drives child resources toward the desired state, then derives
// and patches SnapshotJob status once from the resulting observation. Failed is
// terminal, while Completed keeps retrying successful Job cleanup until it is
// confirmed gone.
func (r *SnapshotJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sj := &snapshotv1alpha1.SnapshotJob{}
	if err := r.Get(ctx, req.NamespacedName, sj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	if snapshotv1alpha1.IsSnapshotJobFailed(sj) {
		return ctrl.Result{}, nil
	}
	if snapshotv1alpha1.IsSnapshotJobCompleted(sj) {
		return r.ensureJobDeleted(ctx, sj)
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
	if meta.IsStatusConditionTrue(desiredStatus.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted) {
		return r.ensureJobDeleted(ctx, sj)
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
		if sj.Status.PodSnapshotName != "" {
			return terminalObservation(snapshotv1alpha1.ReasonJobDeleted,
				fmt.Errorf("source Job %q no longer exists", sj.Name)), ctrl.Result{}, nil
		}
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

// jobFailureReason classifies the Job's failure conditions. FailureTarget is
// considered because Kubernetes publishes it before Failed while terminating
// the Job's pods; Failed takes precedence when both exist.
func jobFailureReason(job *batchv1.Job) (reason string, cause error) {
	for _, conditionType := range []batchv1.JobConditionType{batchv1.JobFailed, batchv1.JobFailureTarget} {
		for _, condition := range job.Status.Conditions {
			if condition.Type != conditionType || condition.Status != corev1.ConditionTrue {
				continue
			}
			if condition.Reason == batchv1.JobReasonDeadlineExceeded {
				return snapshotv1alpha1.ReasonDeadlineExceeded,
					fmt.Errorf("source Job exceeded activeDeadlineSeconds: %s", condition.Message)
			}
			return snapshotv1alpha1.ReasonJobFailed, fmt.Errorf("source Job failed: %s", condition.Message)
		}
	}
	return "", nil
}

// reconcilePodSnapshotResources evaluates PodSnapshot state before Job failure:
// a successful checkpoint can be expected to terminate the source process.
func (r *SnapshotJobReconciler) reconcilePodSnapshotResources(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (snapshotJobObservation, ctrl.Result, error) {
	observed := snapshotJobObservation{job: job}
	snap, err := r.findOwnedPodSnapshot(ctx, sj)
	switch {
	case errors.Is(err, errPodSnapshotNameConflict):
		observed.failure = &snapshotJobFailure{reason: snapshotv1alpha1.ReasonPodSnapshotNameConflict, cause: err}
		return observed, ctrl.Result{}, nil
	case apierrors.IsNotFound(err):
		if reason, cause := jobFailureReason(job); reason != "" {
			observed.failure = &snapshotJobFailure{reason: reason, cause: cause}
			return observed, ctrl.Result{}, nil
		}
		return r.createPodSnapshotForSourceJob(ctx, sj, job)
	case err != nil:
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("find owned PodSnapshot: %w", err)
	}

	observed.podSnapshot = snap
	switch {
	case snapshotv1alpha1.IsPodSnapshotFailed(snap):
		// FailureTarget or Failed can race the PodSnapshot update; re-read through
		// the API reader before deciding which failure is authoritative.
		latestJob := &batchv1.Job{}
		if err := r.sourcePodReader().Get(ctx, client.ObjectKeyFromObject(job), latestJob); err == nil {
			observed.job = latestJob
			if reason, cause := jobFailureReason(latestJob); reason != "" {
				observed.failure = &snapshotJobFailure{reason: reason, cause: cause}
			}
		}
	case snapshotv1alpha1.IsPodSnapshotSucceeded(snap):
		// Capture success is authoritative even if checkpointing terminated the
		// source process and the Job concurrently reports failure.
	default:
		if reason, _ := jobFailureReason(job); reason != "" {
			observed.waitingForCaptureResult = true
			return observed, ctrl.Result{RequeueAfter: captureResultRequeueBackstop}, nil
		}
	}
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
	if failure != nil {
		deriveFailureStatus(next, failure)
		return next.Status
	}
	if observed.waitingForCaptureResult {
		return next.Status
	}
	deriveCompletionStatus(next, observed)
	return next.Status
}

func deriveRunningStatus(next *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation) {
	if observed.job != nil {
		ready := observed.job.Status.Ready != nil && *observed.job.Status.Ready > 0
		captured := observed.podSnapshot != nil && snapshotv1alpha1.IsPodSnapshotSucceeded(observed.podSnapshot)
		if ready || captured {
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
			if failure == nil {
				reason, message := captureFailureReason(observed.podSnapshot)
				failure = &snapshotJobFailure{reason: reason, cause: errors.New(message)}
			}
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
	if meta.FindStatusCondition(next.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning) == nil {
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse,
			snapshotv1alpha1.ReasonPodPending, "source pod was never observed ready before this SnapshotJob failed")
	}
	if !meta.IsStatusConditionTrue(next.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured) {
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionFalse,
			failure.reason, "capture did not complete: "+failure.cause.Error())
	}
	if !meta.IsStatusConditionTrue(next.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted) {
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionCompleted, metav1.ConditionFalse,
			failure.reason, "the SnapshotJob failed before completing: "+failure.cause.Error())
	}
	setCondition(next, snapshotv1alpha1.SnapshotJobConditionFailed, metav1.ConditionTrue,
		failure.reason, failure.cause.Error())
	if next.Status.CompletedAt == nil {
		now := metav1.Now()
		next.Status.CompletedAt = &now
	}
}

func deriveCompletionStatus(next *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation) {
	captured := observed.podSnapshot != nil && snapshotv1alpha1.IsPodSnapshotSucceeded(observed.podSnapshot)
	if captured {
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionCompleted, metav1.ConditionTrue,
			snapshotv1alpha1.ReasonCaptureCompleted, "checkpoint captured; source Job cleanup requested")
		if next.Status.CompletedAt == nil {
			now := metav1.Now()
			next.Status.CompletedAt = &now
		}
	}
}

// captureFailureReason separates bind-stage PodSnapshot failures from failures
// reported by the node agent while capturing the checkpoint.
func captureFailureReason(snap *snapshotv1alpha1.PodSnapshot) (reason, message string) {
	condition := meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	if condition == nil {
		return snapshotv1alpha1.ReasonCaptureFailed, "PodSnapshot Failed=True with no condition detail"
	}
	switch condition.Reason {
	case "ContentConflict", "SourcePodNotFound", "StalePodReference":
		return snapshotv1alpha1.ReasonPodSnapshotFailed, condition.Message
	default:
		return snapshotv1alpha1.ReasonCaptureFailed, condition.Message
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

// ensureJobDeleted deletes the owned source Job after successful completion.
// Failed SnapshotJobs never enter this path, preserving their Jobs for debugging.
func (r *SnapshotJobReconciler) ensureJobDeleted(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (ctrl.Result, error) {
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(job, sj) {
		return ctrl.Result{}, nil
	}
	uid := job.UID
	if err := r.Delete(ctx, job,
		client.Preconditions{UID: &uid},
		client.PropagationPolicy(metav1.DeletePropagationBackground),
	); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete source Job %q: %w", job.Name, err)
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
