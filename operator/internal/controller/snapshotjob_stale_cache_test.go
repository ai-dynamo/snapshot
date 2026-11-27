// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// snapshotJobAPIReader stands for the API server. It counts the SnapshotJob reads.
func snapshotJobAPIReader(s *runtime.Scheme, reads *int, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&snapshotv1alpha1.SnapshotJob{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*snapshotv1alpha1.SnapshotJob); ok {
					*reads++
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
}

func countSourceJobs(t *testing.T, cl client.Client) int {
	t.Helper()
	var jobs batchv1.JobList
	require.NoError(t, cl.List(context.Background(), &jobs))
	return len(jobs.Items)
}

// The cached SnapshotJob records no source Job and the cache holds no Job. That is the first
// reconcile, and it is also a cache that has not seen the first status write yet, after the Job
// that write recorded was deleted. Only the API server's copy tells the two apart.
func TestSnapshotJobReconcileCreatesTheSourceJobOnlyFromACurrentSnapshotJob(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	ctx := context.Background()

	tests := []struct {
		name string
		// api changes the API server's copy after the cache read its own.
		api        func(t *testing.T, api client.Client, sj *snapshotv1alpha1.SnapshotJob)
		wantCreate bool
	}{
		{name: "the cache is current", api: func(*testing.T, client.Client, *snapshotv1alpha1.SnapshotJob) {}, wantCreate: true},
		{name: "the first status write is not in the cache yet", api: func(t *testing.T, api client.Client, sj *snapshotv1alpha1.SnapshotJob) {
			recorded := sj.DeepCopy()
			require.NoError(t, api.Get(ctx, client.ObjectKeyFromObject(sj), recorded))
			recorded.Status.SourceJobUID = types.UID("first-source-job-uid")
			require.NoError(t, api.Status().Update(ctx, recorded))
		}},
		{name: "the SnapshotJob was deleted", api: func(t *testing.T, api client.Client, sj *snapshotv1alpha1.SnapshotJob) {
			require.NoError(t, api.Delete(ctx, sj.DeepCopy()))
		}},
		{name: "the SnapshotJob was created again", api: func(t *testing.T, api client.Client, sj *snapshotv1alpha1.SnapshotJob) {
			require.NoError(t, api.Delete(ctx, sj.DeepCopy()))
			again := minimalSnapshotJob()
			again.UID = types.UID("sj-uid-2")
			require.NoError(t, api.Create(ctx, again))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sj := minimalSnapshotJob()
			r := makeSnapshotJobReconciler(s, sj)
			reads := 0
			api := snapshotJobAPIReader(s, &reads, sj.DeepCopy())
			test.api(t, api, sj)
			reads = 0
			r.NonCacheReadClient = api

			result, err := r.Reconcile(ctx, reconcileRequest(sj))
			require.NoError(t, err)
			assert.Equal(t, 1, reads, "one API read of the SnapshotJob decides the create")

			cached := &snapshotv1alpha1.SnapshotJob{}
			require.NoError(t, r.Get(ctx, reconcileRequest(sj).NamespacedName, cached))
			if test.wantCreate {
				assert.Equal(t, 1, countSourceJobs(t, r.Client))
				assert.NotEmpty(t, cached.Status.Conditions)
				return
			}
			assert.Zero(t, countSourceJobs(t, r.Client), "a stale SnapshotJob must not start the workload")
			assert.Empty(t, cached.Status.Conditions, "nothing is written from a stale SnapshotJob")
			assert.Equal(t, snapshotJobCacheBehindRequeue, result.RequeueAfter)
		})
	}
}

func TestSnapshotJobReconcileRetriesWhenTheSnapshotJobCannotBeConfirmed(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	r := makeSnapshotJobReconciler(s, sj)
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("transient authoritative read failure")
		},
	}).Build()

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.Error(t, err)
	assert.Zero(t, countSourceJobs(t, r.Client), "an unconfirmed SnapshotJob must not start the workload")
}

// staleCachePodSnapshotFixture returns a SnapshotJob that recorded its source Job, with that
// Job and its Pod. The PodSnapshot is the next thing to create. The recorded UID is what
// accepts the Job, so a bare Job is enough.
func staleCachePodSnapshotFixture(t *testing.T, s *runtime.Scheme) (*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod) {
	t.Helper()
	sj := minimalSnapshotJob()
	sj.Status.SourceJobUID = types.UID("source-job-uid")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: sj.Name, Namespace: sj.Namespace, UID: sj.Status.SourceJobUID}}
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	return sj, job, sourcePodForJob(job)
}

// recordPodSnapshot writes the status that the operator writes after it created the
// PodSnapshot.
func recordPodSnapshot(t *testing.T, cl client.Client, sj *snapshotv1alpha1.SnapshotJob) {
	t.Helper()
	recorded := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(sj), recorded))
	recorded.Status.PodSnapshotName = sj.Name
	recorded.Status.PodSnapshotUID = types.UID("first-podsnapshot-uid")
	require.NoError(t, cl.Status().Update(context.Background(), recorded))
}

func countPodSnapshots(t *testing.T, cl client.Client) int {
	t.Helper()
	var snaps snapshotv1alpha1.PodSnapshotList
	require.NoError(t, cl.List(context.Background(), &snaps))
	return len(snaps.Items)
}

// The cached SnapshotJob records no PodSnapshot and the cache holds none. That is the reconcile
// that creates the PodSnapshot, and it is also a cache that has not seen the status write that
// recorded the PodSnapshot yet, after that PodSnapshot was deleted. Only the API server's copy
// tells the two apart.
func TestSnapshotJobReconcileCreatesThePodSnapshotOnlyFromACurrentSnapshotJob(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	ctx := context.Background()

	tests := []struct {
		name string
		// api changes the API server's copy after the cache read its own.
		api        func(t *testing.T, api client.Client, sj *snapshotv1alpha1.SnapshotJob)
		wantCreate bool
	}{
		{name: "the cache is current", api: func(*testing.T, client.Client, *snapshotv1alpha1.SnapshotJob) {}, wantCreate: true},
		{name: "the record of the PodSnapshot is not in the cache yet", api: recordPodSnapshot},
		{name: "the SnapshotJob was deleted", api: func(t *testing.T, api client.Client, sj *snapshotv1alpha1.SnapshotJob) {
			require.NoError(t, api.Delete(ctx, sj.DeepCopy()))
		}},
		{name: "the SnapshotJob was created again", api: func(t *testing.T, api client.Client, sj *snapshotv1alpha1.SnapshotJob) {
			require.NoError(t, api.Delete(ctx, sj.DeepCopy()))
			again := minimalSnapshotJob()
			again.UID = types.UID("sj-uid-2")
			require.NoError(t, api.Create(ctx, again))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sj, job, pod := staleCachePodSnapshotFixture(t, s)
			r := makeSnapshotJobReconciler(s, sj, job, pod)
			reads := 0
			api := snapshotJobAPIReader(s, &reads, sj.DeepCopy(), job.DeepCopy(), pod.DeepCopy())
			test.api(t, api, sj)
			reads = 0
			r.NonCacheReadClient = api

			result, err := r.Reconcile(ctx, reconcileRequest(sj))
			require.NoError(t, err)
			assert.Equal(t, 1, reads, "one API read of the SnapshotJob decides the create")

			cached := &snapshotv1alpha1.SnapshotJob{}
			require.NoError(t, r.Get(ctx, reconcileRequest(sj).NamespacedName, cached))
			if test.wantCreate {
				assert.Equal(t, 1, countPodSnapshots(t, r.Client))
				assert.Equal(t, sj.Name, cached.Status.PodSnapshotName)
				return
			}
			assert.Zero(t, countPodSnapshots(t, r.Client), "a stale SnapshotJob must not create the PodSnapshot again")
			assert.Empty(t, cached.Status.Conditions, "nothing is written from a stale SnapshotJob")
			assert.Equal(t, snapshotJobCacheBehindRequeue, result.RequeueAfter)
		})
	}
}

// A PodSnapshot that was recorded and then deleted ends the SnapshotJob with PodSnapshotDeleted,
// also when the delete reaches the operator before the record reaches its cache.
func TestSnapshotJobReconcileReportsAPodSnapshotDeletedBeforeItsRecordReachedTheCache(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	ctx := context.Background()
	sj, job, pod := staleCachePodSnapshotFixture(t, s)
	r := makeSnapshotJobReconciler(s, sj, job, pod)
	reads := 0
	api := snapshotJobAPIReader(s, &reads, sj.DeepCopy(), job.DeepCopy(), pod.DeepCopy())
	recordPodSnapshot(t, api, sj)
	r.NonCacheReadClient = api

	_, err := r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)
	require.Zero(t, countPodSnapshots(t, r.Client), "the deleted PodSnapshot must not come back")

	// The watch event with the record arrives.
	recordPodSnapshot(t, r.Client, sj)
	_, err = r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(ctx, reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, metav1.ConditionTrue, failed.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonPodSnapshotDeleted, failed.Reason)
	assert.Zero(t, countPodSnapshots(t, r.Client))
}

// The API read belongs to the create. A reconcile that still waits for the source Pod has
// nothing to create, so it reads the SnapshotJob from the cache only.
func TestSnapshotJobReconcileWaitsForTheSourcePodWithoutAnAPIRead(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj, job, _ := staleCachePodSnapshotFixture(t, s)
	r := makeSnapshotJobReconciler(s, sj, job)
	reads := 0
	r.NonCacheReadClient = snapshotJobAPIReader(s, &reads, sj.DeepCopy(), job.DeepCopy())

	result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Zero(t, reads, "no API read of the SnapshotJob while there is no Pod to capture")
	assert.Zero(t, countPodSnapshots(t, r.Client))
	assert.Equal(t, sourcePodRequeueBackstop, result.RequeueAfter)
}

func TestSnapshotJobReconcileRetriesWhenTheSnapshotJobCannotBeConfirmedForItsPodSnapshot(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj, job, pod := staleCachePodSnapshotFixture(t, s)
	r := makeSnapshotJobReconciler(s, sj, job, pod)
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("transient authoritative read failure")
		},
	}).Build()

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.Error(t, err)
	assert.Zero(t, countPodSnapshots(t, r.Client), "an unconfirmed SnapshotJob must not create the PodSnapshot")
}
