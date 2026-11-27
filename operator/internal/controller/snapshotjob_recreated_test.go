// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// sourceJobOfDeletedSnapshotJob returns the source Job of a SnapshotJob that had sj's name and is
// gone. The garbage collector removes that Job a moment after its SnapshotJob.
func sourceJobOfDeletedSnapshotJob(t *testing.T, sj *snapshotv1alpha1.SnapshotJob) *batchv1.Job {
	t.Helper()
	deleted := sj.DeepCopy()
	deleted.UID = types.UID("deleted-sj-uid")
	job, err := buildSourceJob(deleted)
	require.NoError(t, err)
	job.UID = types.UID("deleted-sj-job-uid")
	require.NoError(t, controllerutil.SetControllerReference(deleted, job, snapshotJobReconcilerScheme()))
	return job
}

// A SnapshotJob that is deleted and created again under its name can find the source Job of the
// deleted one. That name is being freed, so it is not a conflict: the new SnapshotJob waits, and
// creates its own source Job when the old one is gone.
func TestSnapshotJobReconcileWaitsForTheSourceJobOfADeletedSnapshotJob(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	ctx := context.Background()
	sj := minimalSnapshotJob()
	leftover := sourceJobOfDeletedSnapshotJob(t, sj)
	r := makeSnapshotJobReconciler(s, sj, leftover)

	result, err := r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)

	assert.Equal(t, snapshotJobCacheBehindRequeue, result.RequeueAfter)
	waiting := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(ctx, reconcileRequest(sj).NamespacedName, waiting))
	assert.Empty(t, waiting.Status.Conditions, "a name that is being freed is not a conflict")
	assert.Equal(t, leftover.UID, getSourceJob(t, r.Client, sj).UID, "the Job of the deleted SnapshotJob is left to the garbage collector")

	require.NoError(t, r.Delete(ctx, leftover))
	_, err = r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)

	created := getSourceJob(t, r.Client, sj)
	assert.True(t, metav1.IsControlledBy(created, sj), "the new SnapshotJob creates its own source Job")
	started := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(ctx, reconcileRequest(sj).NamespacedName, started))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(started))
}

// Only the Job of an earlier SnapshotJob of this name is waited for. Every other Job at the name
// stays a conflict, as before.
func TestSnapshotJobReconcileKeepsTheNameConflictForOtherJobs(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	ctx := context.Background()

	tests := []struct {
		name   string
		change func(sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job)
	}{
		{name: "the Job has no controller", change: func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) {
			job.OwnerReferences = nil
		}},
		{name: "the controller is another kind", change: func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) {
			job.OwnerReferences[0].APIVersion = "batch/v1"
			job.OwnerReferences[0].Kind = "CronJob"
		}},
		{name: "the controller is a SnapshotJob of another name", change: func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) {
			job.OwnerReferences[0].Name = "another-snapshotjob"
		}},
		{name: "this SnapshotJob recorded its own source Job", change: func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job) {
			sj.Status.SourceJobUID = types.UID("recorded-job-uid")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sj := minimalSnapshotJob()
			job := sourceJobOfDeletedSnapshotJob(t, sj)
			require.Equal(t, ptr.To(true), job.OwnerReferences[0].Controller)
			test.change(sj, job)
			r := makeSnapshotJobReconciler(s, sj, job)

			_, err := r.Reconcile(ctx, reconcileRequest(sj))
			require.NoError(t, err)

			updated := &snapshotv1alpha1.SnapshotJob{}
			require.NoError(t, r.Get(ctx, reconcileRequest(sj).NamespacedName, updated))
			failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, snapshotv1alpha1.ReasonJobNameConflict, failed.Reason)
		})
	}
}
