// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func jobForCacheTest(owners ...metav1.OwnerReference) *batchv1.Job {
	now := metav1.Now()
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "warm-worker",
			Namespace:         "inference",
			UID:               "job-uid",
			Labels:            map[string]string{"l": "v"},
			Annotations:       map[string]string{"a": "true"},
			OwnerReferences:   owners,
			DeletionTimestamp: &now,
			ManagedFields:     []metav1.ManagedFieldsEntry{{Manager: "kube-controller-manager"}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "worker"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "worker", Image: "img", Env: []corev1.EnvVar{{Name: "K", Value: "V"}}}},
					Volumes:    []corev1.Volume{{Name: "v"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Active:     1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}},
		},
	}
}

func TestTrimJobForCacheTrimsJobsNoSnapshotJobControls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		owners []metav1.OwnerReference
	}{
		{"no owner", nil},
		{"controlled by a CronJob", []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "CronJob", Name: "nightly", UID: "cron-uid", Controller: ptr.To(true),
		}}},
		{"SnapshotJob owner that is not the controller", []metav1.OwnerReference{{
			APIVersion: snapshotv1alpha1.GroupVersion.String(), Kind: snapshotJobKind, Name: "warm-worker", UID: "sj-uid",
		}}},
		{"controlled by a SnapshotJob kind of another group", []metav1.OwnerReference{{
			APIVersion: "example.com/v1", Kind: snapshotJobKind, Name: "warm-worker", UID: "other-uid", Controller: ptr.To(true),
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := jobForCacheTest(tc.owners...)
			want := *job.ObjectMeta.DeepCopy()
			want.ManagedFields = nil

			out, err := TrimJobForCache(job)
			require.NoError(t, err)
			got := out.(*batchv1.Job)

			assert.Equal(t, want, got.ObjectMeta, "metadata other than managedFields must be kept")
			assert.Equal(t, batchv1.JobSpec{}, got.Spec)
			assert.Equal(t, batchv1.JobStatus{}, got.Status)
		})
	}
}

func TestTrimJobForCacheKeepsJobsASnapshotJobControls(t *testing.T) {
	controlledBy := func(apiVersion string) metav1.OwnerReference {
		return metav1.OwnerReference{
			APIVersion: apiVersion, Kind: snapshotJobKind, Name: "warm-worker", UID: "sj-uid", Controller: ptr.To(true),
		}
	}
	for _, tc := range []struct {
		name   string
		owners []metav1.OwnerReference
	}{
		{"controlled by a SnapshotJob", []metav1.OwnerReference{controlledBy(snapshotv1alpha1.GroupVersion.String())}},
		// A Job keeps the owner reference it was created with, so a Job that an
		// earlier API version of SnapshotJob created must still be kept.
		{"controlled by a SnapshotJob of another API version", []metav1.OwnerReference{
			controlledBy(snapshotv1alpha1.GroupVersion.Group + "/v1"),
		}},
		{"SnapshotJob controller listed after another owner", []metav1.OwnerReference{
			{APIVersion: "v1", Kind: "ConfigMap", Name: "settings", UID: "cm-uid"},
			controlledBy(snapshotv1alpha1.GroupVersion.String()),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := jobForCacheTest(tc.owners...)
			want := job.DeepCopy()
			want.ManagedFields = nil

			out, err := TrimJobForCache(job)
			require.NoError(t, err)
			assert.Equal(t, want, out.(*batchv1.Job))
		})
	}
}

// The reconciler creates the source Job here, so the owner reference under
// test is the one createSourceJob writes.
func TestTrimJobForCacheKeepsTheSourceJobTheOperatorCreates(t *testing.T) {
	sj := minimalSnapshotJob()
	r := makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj)
	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	job := getSourceJob(t, r.Client, sj)
	require.NotEmpty(t, job.Spec.Template.Spec.Containers)
	job.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "manager"}}
	want := job.DeepCopy()
	want.ManagedFields = nil

	out, err := TrimJobForCache(job)
	require.NoError(t, err)
	assert.Equal(t, want, out.(*batchv1.Job))
}

// trimJobsOnRead sends every Job a reconciler reads through TrimJobForCache,
// as the manager's cache does before a controller sees the Job.
func trimJobsOnRead() interceptor.Funcs {
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		_, err := TrimJobForCache(obj)
		return err
	}}
}

func snapshotJobFailedReason(t *testing.T, r *SnapshotJobReconciler, sj *snapshotv1alpha1.SnapshotJob) string {
	t.Helper()
	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	require.Equal(t, metav1.ConditionTrue, failed.Status)
	return failed.Reason
}

// A SnapshotJob reads the spec of its own source Job to accept it and the
// status to follow it, so both must come back from the trimmed cache.
func TestSnapshotJobReconcileFollowsItsSourceJobThroughTheTrimmedCache(t *testing.T) {
	ctx := context.Background()
	sj := minimalSnapshotJob()
	r := makeSnapshotJobReconcilerWithInterceptor(snapshotJobReconcilerScheme(), trimJobsOnRead(), sj)
	_, err := r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)

	// The Job controller reports that the deadline passed.
	job := getSourceJob(t, r.Client, sj)
	setJobFailureCondition(job, batchv1.JobFailed, batchv1.JobReasonDeadlineExceeded, "Job was active longer than specified deadline")
	require.NoError(t, r.Status().Update(ctx, job))

	_, err = r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, snapshotv1alpha1.ReasonDeadlineExceeded, snapshotJobFailedReason(t, r, sj))
}

// The name-conflict decision is the one cached read of a Job that no
// SnapshotJob controls, so it must not need the spec or status of that Job.
func TestSnapshotJobReconcileReportsANameConflictThroughTheTrimmedCache(t *testing.T) {
	sj := minimalSnapshotJob()
	foreign := jobForCacheTest()
	// The fake client stores neither a deletion timestamp without a finalizer
	// nor a managedFields entry without an operation.
	foreign.DeletionTimestamp, foreign.ManagedFields = nil, nil
	r := makeSnapshotJobReconcilerWithInterceptor(snapshotJobReconcilerScheme(), trimJobsOnRead(), sj, foreign)

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, snapshotv1alpha1.ReasonJobNameConflict, snapshotJobFailedReason(t, r, sj))
}

func TestTrimJobForCacheLeavesOtherObjects(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	out, err := TrimJobForCache(pod)
	require.NoError(t, err)
	assert.Same(t, pod, out)
}
