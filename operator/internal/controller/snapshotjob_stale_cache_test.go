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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
