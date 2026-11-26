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
	r.DefaultStoreName = "default"
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

func TestSnapshotJobStartsWhenTheStoreIsReady(t *testing.T) {
	sj := minimalSnapshotJob()
	r := bindingJobReconciler(makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj, readyStore("default")))

	updated := snapshotJobAfterReconcile(t, r, sj)

	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
	assert.Equal(t, 1, sourceJobCount(t, r))
}

func TestSnapshotJobWaitsForAStoreThatIsNotReady(t *testing.T) {
	notReady := readyStore("default")
	notReady.Status.Conditions[0].Status = metav1.ConditionFalse
	notReady.Status.Conditions[0].Reason = ReasonStoreUnreachable
	notReady.Status.Conditions[0].Message = "write: denied"
	sj := minimalSnapshotJob()
	r := bindingJobReconciler(makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj, notReady))

	updated := snapshotJobAfterReconcile(t, r, sj)

	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated), "a store that is not ready is a wait, not a failure")
	running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running)
	assert.Equal(t, metav1.ConditionFalse, running.Status)
	assert.Equal(t, ReasonStoreNotReady, running.Reason)
	assert.Contains(t, running.Message, "StoreUnreachable: write: denied")
	assert.Zero(t, sourceJobCount(t, r), "no source pod starts before the store can be used")
}

func TestSnapshotJobFailsForAStoreThatIsMissingOrTerminating(t *testing.T) {
	terminating := readyStore("default")
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminating.Finalizers = []string{SnapshotStoreProtectionFinalizer}
	for _, tc := range []struct {
		name    string
		store   *snapshotv1alpha1.SnapshotStore
		message string
	}{
		{"missing", nil, ReasonStoreNotFound},
		{"terminating", terminating, ReasonStoreTerminating},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sj := minimalSnapshotJob()
			objects := []client.Object{sj}
			if tc.store != nil {
				objects = append(objects, tc.store)
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

func TestSnapshotJobUsesTheStoreItsTemplateNames(t *testing.T) {
	sj := minimalSnapshotJob()
	sj.Spec.PodSnapshotTemplate.SnapshotStoreName = "gold"
	r := bindingJobReconciler(makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj, readyStore("gold")))

	snapshotJobAfterReconcile(t, r, sj)

	assert.Equal(t, 1, sourceJobCount(t, r), "the default store does not exist, and the named one is ready")
}

func TestSnapshotJobBindingOffWaitsForAStoreOtherThanTheDefault(t *testing.T) {
	sj := minimalSnapshotJob()
	sj.Spec.PodSnapshotTemplate.SnapshotStoreName = "gold"
	r := makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj, readyStore("gold"))
	r.DefaultStoreName = "default"

	updated := snapshotJobAfterReconcile(t, r, sj)

	running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running)
	assert.Equal(t, ReasonStoreNotReady, running.Reason)
	assert.Contains(t, running.Message, "not bound to a store")
	assert.Zero(t, sourceJobCount(t, r))
}

func TestSnapshotJobBindingOffStartsWithoutAStoreObject(t *testing.T) {
	sj := minimalSnapshotJob()
	r := makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj)
	r.DefaultStoreName = "default"

	snapshotJobAfterReconcile(t, r, sj)

	assert.Equal(t, 1, sourceJobCount(t, r))
}

func TestPodSnapshotFromAJobCarriesTheTemplateStore(t *testing.T) {
	sj := minimalSnapshotJob()
	sj.Spec.PodSnapshotTemplate.SnapshotStoreName = "gold"
	pod := sourcePodForJob(&batchv1.Job{})
	pod.Name = "source"

	snap, err := buildPodSnapshot(sj, pod)

	require.NoError(t, err)
	assert.Equal(t, "gold", snap.Spec.SnapshotStoreName)
}

func TestStoreChangeWakesOnlyTheJobsThatHaveNotStarted(t *testing.T) {
	unnamed := minimalSnapshotJob()
	unnamed.Name = "unnamed"
	named := minimalSnapshotJob()
	named.Name = "named"
	named.Spec.PodSnapshotTemplate.SnapshotStoreName = "gold"
	started := minimalSnapshotJob()
	started.Name = "started"
	started.Status.SourceJobUID = "job-uid"
	r := bindingJobReconciler(makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), unnamed, named, started))

	byDefault := r.storeToWaitingSnapshotJobs(context.Background(), readyStore("default"))
	byName := r.storeToWaitingSnapshotJobs(context.Background(), readyStore("gold"))

	require.Len(t, byDefault, 1)
	assert.Equal(t, "unnamed", byDefault[0].Name)
	require.Len(t, byName, 1)
	assert.Equal(t, "named", byName[0].Name)
}
