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
	if !metav1.IsControlledBy(job, sj) {
		return terminalObservation(snapshotv1alpha1.ReasonJobNameConflict,
			fmt.Errorf("an object not controlled by this SnapshotJob already holds the name %q", sj.Name)), ctrl.Result{}, nil
	}
	return r.reconcilePodSnapshotResources(ctx, sj, job)
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

// snapshotJobTerminalFailure combines the source Job and PodSnapshot terminal
// signals. A source Job failure always makes the two-signal SnapshotJob fail.
// Successful Job completion with a pending capture remains nonterminal: the
// PodSnapshot controller resolves that state from source-pod and content events.
func snapshotJobTerminalFailure(job *batchv1.Job, snap *snapshotv1alpha1.PodSnapshot) *snapshotJobFailure {
	terminal := classifySourceJobTerminal(job)
	if terminal.failure != nil {
		return terminal.failure
	}
	if snap != nil && snapshotv1alpha1.IsPodSnapshotFailed(snap) {
		reason, message := captureFailureReason(snap)
		return &snapshotJobFailure{reason: reason, cause: errors.New(message)}
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
