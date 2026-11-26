// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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

func TestTrimJobForCacheKeepsSourceJobs(t *testing.T) {
	sj := minimalSnapshotJob()
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	// The same call createSourceJob makes, so the kept Jobs are the ones the
	// operator creates.
	require.NoError(t, controllerutil.SetControllerReference(sj, job, snapshotJobReconcilerScheme()))
	job.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "manager"}}
	job.Status = batchv1.JobStatus{
		Active:     1,
		Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}},
	}
	want := job.DeepCopy()
	want.ManagedFields = nil

	out, err := TrimJobForCache(job)
	require.NoError(t, err)
	assert.Equal(t, want, out.(*batchv1.Job))
}

// The name-conflict decision is the one cached read of a Job that no
// SnapshotJob controls, so a trimmed Job must still produce it.
func TestTrimJobForCacheKeepsTheSourceJobClassification(t *testing.T) {
	sj := minimalSnapshotJob()

	foreign, err := TrimJobForCache(jobForCacheTest())
	require.NoError(t, err)
	failure := classifyExistingSourceJob(sj, foreign.(*batchv1.Job))
	require.NotNil(t, failure)
	assert.Equal(t, snapshotv1alpha1.ReasonJobNameConflict, failure.reason)

	owned, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, owned, snapshotJobReconcilerScheme()))
	trimmed, err := TrimJobForCache(owned)
	require.NoError(t, err)
	assert.Nil(t, classifyExistingSourceJob(sj, trimmed.(*batchv1.Job)))
}

func TestTrimJobForCacheLeavesOtherObjects(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	out, err := TrimJobForCache(pod)
	require.NoError(t, err)
	assert.Same(t, pod, out)
}
