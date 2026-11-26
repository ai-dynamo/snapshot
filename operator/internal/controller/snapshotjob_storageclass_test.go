// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func bindingJobReconciler(r *SnapshotJobReconciler) *SnapshotJobReconciler {
	r.BindNewContents = true
	r.DefaultClassName = "default"
	return r
}

func snapshotJobAfterReconcile(t *testing.T, r *SnapshotJobReconciler, sj *snapshotv1alpha1.SnapshotJob) *snapshotv1alpha1.SnapshotJob {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	return updated
}

func sourceJobCount(t *testing.T, r *SnapshotJobReconciler) int {
	t.Helper()
	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	return len(jobs.Items)
}

func TestSnapshotJobStartsWhenTheClassIsReady(t *testing.T) {
	sj := minimalSnapshotJob()
	r := bindingJobReconciler(makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj, readyClass("default")))

	updated := snapshotJobAfterReconcile(t, r, sj)

	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
	assert.Equal(t, 1, sourceJobCount(t, r))
}

func TestSnapshotJobWaitsForAClassThatIsNotReady(t *testing.T) {
	notReady := readyClass("default")
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse
	notReady.Status.Conditions[0].Reason = ReasonStoreUnreachable
	notReady.Status.Conditions[0].Message = "write: denied"
	sj := minimalSnapshotJob()
	r := bindingJobReconciler(makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj, notReady))

	updated := snapshotJobAfterReconcile(t, r, sj)

	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated), "a class that is not ready is a wait, not a failure")
	running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running)
	assert.Equal(t, metav1.ConditionFalse, running.Status)
	assert.Equal(t, ReasonStorageClassNotReady, running.Reason)
	assert.Contains(t, running.Message, "StoreUnreachable: write: denied")
	assert.Zero(t, sourceJobCount(t, r), "no source pod starts before the class can be used")
}

func TestSnapshotJobFailsForAClassThatIsMissingOrTerminating(t *testing.T) {
	terminating := readyClass("default")
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminating.Finalizers = []string{StorageClassProtectionFinalizer}
	for _, tc := range []struct {
		name    string
		class   *snapshotv1alpha1.SnapshotStorageClass
		message string
	}{
		{"missing", nil, ReasonStorageClassNotFound},
		{"terminating", terminating, ReasonStorageClassTerminating},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sj := minimalSnapshotJob()
			objects := []client.Object{sj}
			if tc.class != nil {
				objects = append(objects, tc.class)
			}
			r := bindingJobReconciler(makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), objects...))

			updated := snapshotJobAfterReconcile(t, r, sj)

			assert.True(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
			failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, snapshotv1alpha1.ReasonInvalidSpec, failed.Reason)
			assert.Contains(t, failed.Message, tc.message)
			assert.Zero(t, sourceJobCount(t, r))
		})
	}
}

func TestSnapshotJobUsesTheClassItsTemplateNames(t *testing.T) {
	sj := minimalSnapshotJob()
	sj.Spec.PodSnapshotTemplate.StorageClassName = "gold"
	r := bindingJobReconciler(makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj, readyClass("gold")))

	snapshotJobAfterReconcile(t, r, sj)

	assert.Equal(t, 1, sourceJobCount(t, r), "the default class does not exist, and the named one is ready")
}

func TestSnapshotJobBindingOffWaitsForAClassOtherThanTheDefault(t *testing.T) {
	sj := minimalSnapshotJob()
	sj.Spec.PodSnapshotTemplate.StorageClassName = "gold"
	r := makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj, readyClass("gold"))
	r.DefaultClassName = "default"

	updated := snapshotJobAfterReconcile(t, r, sj)

	running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running)
	assert.Equal(t, ReasonStorageClassNotReady, running.Reason)
	assert.Contains(t, running.Message, "not bound to a store")
	assert.Zero(t, sourceJobCount(t, r))
}

func TestSnapshotJobBindingOffStartsWithoutAClassObject(t *testing.T) {
	sj := minimalSnapshotJob()
	r := makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj)
	r.DefaultClassName = "default"

	snapshotJobAfterReconcile(t, r, sj)

	assert.Equal(t, 1, sourceJobCount(t, r))
}

func TestPodSnapshotFromAJobCarriesTheTemplateClass(t *testing.T) {
	sj := minimalSnapshotJob()
	sj.Spec.PodSnapshotTemplate.StorageClassName = "gold"
	pod := sourcePodForJob(&batchv1.Job{})
	pod.Name = "source"

	snap, err := buildPodSnapshot(sj, pod)

	require.NoError(t, err)
	assert.Equal(t, "gold", snap.Spec.StorageClassName)
}

func TestClassChangeWakesOnlyTheJobsThatHaveNotStarted(t *testing.T) {
	unnamed := minimalSnapshotJob()
	unnamed.Name = "unnamed"
	named := minimalSnapshotJob()
	named.Name = "named"
	named.Spec.PodSnapshotTemplate.StorageClassName = "gold"
	started := minimalSnapshotJob()
	started.Name = "started"
	started.Status.SourceJobUID = "job-uid"
	r := bindingJobReconciler(makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), unnamed, named, started))

	byDefault := r.classToWaitingSnapshotJobs(context.Background(), readyClass("default"))
	byName := r.classToWaitingSnapshotJobs(context.Background(), readyClass("gold"))

	require.Len(t, byDefault, 1)
	assert.Equal(t, "unnamed", byDefault[0].Name)
	require.Len(t, byName, 1)
	assert.Equal(t, "named", byName[0].Name)
}
