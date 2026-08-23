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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

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
	return r.reconcileExistingSourceJob(ctx, sj, job)
}

// reconcileExistingSourceJob applies the one-shot Job identity gate shared by
// steady-state reconciliation and Create-AlreadyExists recovery.
func (r *SnapshotJobReconciler) reconcileExistingSourceJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (snapshotJobObservation, ctrl.Result, error) {
	if failure := classifyExistingSourceJob(sj, job); failure != nil {
		return snapshotJobObservation{failure: failure}, ctrl.Result{}, nil
	}
	return r.reconcileAcceptedSourceJob(ctx, sj, job)
}

// reconcileAcceptedSourceJob continues after validating the source Job.
func (r *SnapshotJobReconciler) reconcileAcceptedSourceJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (snapshotJobObservation, ctrl.Result, error) {
	if sj.Status.SourceJobUID == "" && job.UID != "" {
		// Bind the one-shot Job incarnation before creating or adopting any
		// downstream capture resource.
		return snapshotJobObservation{job: job}, ctrl.Result{}, nil
	}
	return r.reconcilePodSnapshotResources(ctx, sj, job)
}

// readAuthoritativeSourceJob directly reads and classifies the source Job.
func (r *SnapshotJobReconciler) readAuthoritativeSourceJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (*batchv1.Job, *snapshotJobFailure, error) {
	job := &batchv1.Job{}
	if err := r.NonCacheReadClient.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, sourceJobDeletedFailure(sj), nil
		}
		return nil, nil, err
	}
	if failure := classifyExistingSourceJob(sj, job); failure != nil {
		return job, failure, nil
	}
	return job, nil, nil
}

func sourceJobDeletedFailure(sj *snapshotv1alpha1.SnapshotJob) *snapshotJobFailure {
	return &snapshotJobFailure{
		reason: snapshotv1alpha1.ReasonJobDeleted,
		cause:  fmt.Errorf("source Job %q (uid %q) no longer exists", sj.Name, sj.Status.SourceJobUID),
	}
}

// classifyExistingSourceJob validates ownership and one-shot Job identity.
func classifyExistingSourceJob(sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) *snapshotJobFailure {
	if !metav1.IsControlledBy(job, sj) {
		return &snapshotJobFailure{
			reason: snapshotv1alpha1.ReasonJobNameConflict,
			cause:  fmt.Errorf("an object not controlled by this SnapshotJob already holds the name %q", sj.Name),
		}
	}
	if expectedUID := sj.Status.SourceJobUID; expectedUID != "" {
		if job.UID != expectedUID {
			return &snapshotJobFailure{
				reason: snapshotv1alpha1.ReasonJobNameConflict,
				cause: fmt.Errorf("source Job %q was replaced: found uid %q, expected %q",
					job.Name, job.UID, expectedUID),
			}
		}
		return nil
	}

	desired, err := buildSourceJob(sj)
	if err != nil {
		return &snapshotJobFailure{reason: snapshotv1alpha1.ReasonInvalidSpec, cause: err}
	}
	targetContainer, err := snapshotJobTargetContainer(sj)
	if err != nil {
		return &snapshotJobFailure{reason: snapshotv1alpha1.ReasonInvalidSpec, cause: err}
	}
	if !sourceJobHasExpectedIdentity(desired, job, targetContainer) {
		return &snapshotJobFailure{
			reason: snapshotv1alpha1.ReasonJobNameConflict,
			cause: fmt.Errorf("existing source Job %q does not carry the immutable identity expected for this SnapshotJob",
				job.Name),
		}
	}
	return nil
}

// sourceJobHasExpectedIdentity checks only the immutable identity and capture
// protocol fields needed for safe adoption. API defaults and unrelated
// admission mutations are deliberately outside this narrow recovery check.
func sourceJobHasExpectedIdentity(desired, actual *batchv1.Job, targetContainer string) bool {
	for _, key := range []string{
		snapshotv1alpha1.SnapshotJobOwnerLabel,
		snapshotv1alpha1.SnapshotJobOwnerUIDLabel,
		snapshotv1alpha1.CheckpointSourceLabel,
	} {
		if actual.Spec.Template.Labels[key] != desired.Spec.Template.Labels[key] {
			return false
		}
	}
	for i := range actual.Spec.Template.Spec.Containers {
		if actual.Spec.Template.Spec.Containers[i].Name == targetContainer {
			return true
		}
	}
	return false
}

type sourceJobTerminalState int

const (
	sourceJobActive sourceJobTerminalState = iota
	sourceJobComplete
	sourceJobFailed
	sourceJobDeadlineExceeded
)

type sourceJobTerminalResult struct {
	state   sourceJobTerminalState
	failure *snapshotJobFailure
}

// classifySourceJobTerminal returns one coherent terminal result. An explicit
// deadline has the highest precedence, then Failed, FailureTarget, and Complete.
func classifySourceJobTerminal(job *batchv1.Job) sourceJobTerminalResult {
	for i := range job.Status.Conditions {
		condition := &job.Status.Conditions[i]
		if condition.Status == corev1.ConditionTrue &&
			(condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobFailureTarget) &&
			condition.Reason == batchv1.JobReasonDeadlineExceeded {
			return sourceJobTerminalResult{
				state: sourceJobDeadlineExceeded,
				failure: &snapshotJobFailure{
					reason: snapshotv1alpha1.ReasonDeadlineExceeded,
					cause:  fmt.Errorf("source Job exceeded activeDeadlineSeconds: %s", condition.Message),
				},
			}
		}
	}
	for _, conditionType := range []batchv1.JobConditionType{batchv1.JobFailed, batchv1.JobFailureTarget} {
		for i := range job.Status.Conditions {
			condition := &job.Status.Conditions[i]
			if condition.Type != conditionType || condition.Status != corev1.ConditionTrue {
				continue
			}
			return sourceJobTerminalResult{
				state: sourceJobFailed,
				failure: &snapshotJobFailure{
					reason: snapshotv1alpha1.ReasonJobFailed,
					cause:  fmt.Errorf("source Job failed: %s", condition.Message),
				},
			}
		}
	}
	for i := range job.Status.Conditions {
		condition := &job.Status.Conditions[i]
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return sourceJobTerminalResult{state: sourceJobComplete}
		}
	}
	return sourceJobTerminalResult{state: sourceJobActive}
}

// snapshotJobTerminalFailure arbitrates the terminal signals. Once a capture
// exists, its result is authoritative: Ready is success regardless of how the
// source Job ended (the checkpoint terminates the source process, so a failed
// source Job is the expected outcome of a successful capture), a Failed capture
// keeps its specific reason even when the Job also failed, and a pending
// capture is never failed on the source Job's word alone — the caller waits for
// the agent's terminal result. Only before any capture exists does a source Job
// failure fail the SnapshotJob immediately. One exception on the failure side:
// an explicit deadline expiry is the root cause of a capture that died with it,
// so DeadlineExceeded wins over the collateral capture failure.
func snapshotJobTerminalFailure(job *batchv1.Job, snap *snapshotv1alpha1.PodSnapshot) *snapshotJobFailure {
	if snap != nil {
		if snapshotv1alpha1.IsPodSnapshotFailed(snap) {
			if terminal := classifySourceJobTerminal(job); terminal.state == sourceJobDeadlineExceeded {
				return terminal.failure
			}
			reason, message := captureFailureReason(snap)
			return &snapshotJobFailure{reason: reason, cause: errors.New(message)}
		}
		return nil
	}
	return classifySourceJobTerminal(job).failure
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
	if expectedUID := sj.Status.SourceJobUID; expectedUID != "" && job.UID != expectedUID {
		r.Recorder.Eventf(sj, corev1.EventTypeWarning, snapshotv1alpha1.ReasonJobNameConflict,
			"Refusing to delete replacement source Job %q with uid %q; expected %q", job.Name, job.UID, expectedUID)
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
