// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// The Pod cache can be behind the API server, and a pod watch that lists again after a gap can
// skip a pod that was created and deleted in the gap. The tests in this file give the reconciler
// a cache and an API server that hold different source pods, and change one without the other.

// makeSnapshotReconcilerBehindAPIServer builds a reconciler whose cache holds the cached pod and
// whose API server holds the live pod; either may be nil. It returns the API server's client so
// that a test can change what is there.
func makeSnapshotReconcilerBehindAPIServer(s *runtime.Scheme, snap *snapshotv1alpha1.PodSnapshot, cached, live *corev1.Pod) (*PodSnapshotReconciler, client.Client) {
	objs := []client.Object{snap}
	if cached != nil {
		objs = append(objs, cached)
	}
	r := makeSnapshotReconciler(s, objs...)
	builder := fake.NewClientBuilder().WithScheme(s)
	if live != nil {
		builder = builder.WithObjects(live.DeepCopy())
	}
	apiServer := builder.Build()
	r.NonCacheReadClient = apiServer
	return r, apiServer
}

// sourcePodWithUID builds the source pod "worker-0" with the given UID. An empty node name is a
// pod the scheduler has not placed yet.
func sourcePodWithUID(uid types.UID, nodeName string) *corev1.Pod {
	pod := scheduledPod()
	pod.UID = uid
	pod.Spec.NodeName = nodeName
	return pod
}

// deliverSourcePodToCache makes the cache hold what the API server holds for the source pod, as
// the pod watch does once it catches up.
func deliverSourcePodToCache(t *testing.T, r *PodSnapshotReconciler, apiServer client.Client) {
	t.Helper()
	ctx := context.Background()
	key := client.ObjectKeyFromObject(scheduledPod())
	cached := &corev1.Pod{}
	if err := r.Get(ctx, key, cached); err == nil {
		require.NoError(t, r.Delete(ctx, cached))
	}
	live := &corev1.Pod{}
	if err := apiServer.Get(ctx, key, live); err == nil {
		live.ResourceVersion = ""
		require.NoError(t, r.Create(ctx, live))
	}
}

// requireWaitsForCache reconciles a PodSnapshot whose source pod is in the API server and not in
// the cache. That pass must decide nothing on the API server's pod: no failure, no binding, no
// status write. It must ask for the backstop requeue, because no pod event is certain to follow.
func requireWaitsForCache(t *testing.T, r *PodSnapshotReconciler, snap *snapshotv1alpha1.PodSnapshot) {
	t.Helper()
	ctx := context.Background()
	before := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), before))

	res := reconcileSnapshot(t, r, snap.Name)
	assert.Equal(t, sourcePodCacheBackstop, res.RequeueAfter,
		"a wait on a pod the cache does not hold must not depend on a pod event")

	after := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), after))
	failed := meta.FindStatusCondition(after.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.Nil(t, failed, "failed for good although the source pod exists: %+v", failed)
	assert.Nil(t, after.Status.BoundPodSnapshotContentName, "bound to a pod the cache does not hold")
	var contents snapshotv1alpha1.PodSnapshotContentList
	require.NoError(t, r.List(ctx, &contents))
	assert.Empty(t, contents.Items, "created a work order for a pod the cache does not hold")
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"a write would wake the PodSnapshot again, and every pass of this wait reads the API server")
}

func requireFailedWithReason(t *testing.T, r *PodSnapshotReconciler, snap *snapshotv1alpha1.PodSnapshot, reason string) *snapshotv1alpha1.PodSnapshot {
	t.Helper()
	updated := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, failed, "a PodSnapshot of a pod that is gone must fail")
	assert.Equal(t, reason, failed.Reason)
	return updated
}

// A source pod that exists must not fail a PodSnapshot for good because the cache does not hold
// it yet, or still holds the pod it replaced: Failed is sticky. The PodSnapshot waits, and goes
// on from the cached pod once the pod watch has delivered it.
func TestSnapshotReconciler_SourcePodMissingFromCacheIsNotFailed(t *testing.T) {
	s := snapshotReconcilerScheme()
	for _, tc := range []struct {
		name       string
		pinnedUID  types.UID
		cached     *corev1.Pod
		live       *corev1.Pod
		wantReason string
	}{
		{
			name: "pod not in the cache yet",
			live: sourcePodWithUID("pod-uid-9", "node-a"),
		},
		{
			name:       "pod not in the cache yet and not scheduled",
			live:       sourcePodWithUID("pod-uid-9", ""),
			wantReason: podSnapshotReasonSourcePodUnscheduled,
		},
		{
			name:      "cache still holds the pod that the pinned one replaced",
			pinnedUID: "pod-uid-9",
			cached:    sourcePodWithUID("pod-uid-8", "node-a"),
			live:      sourcePodWithUID("pod-uid-9", "node-b"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := makeSnapshotForReconcile()
			snap.Spec.Source.PodRef.UID = tc.pinnedUID
			r, apiServer := makeSnapshotReconcilerBehindAPIServer(s, snap, tc.cached, tc.live)

			requireWaitsForCache(t, r, snap)

			deliverSourcePodToCache(t, r, apiServer)
			res := reconcileSnapshot(t, r, snap.Name)
			assert.Zero(t, res.RequeueAfter, "the pod watch wakes a PodSnapshot whose pod the cache holds")

			updated := &snapshotv1alpha1.PodSnapshot{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), updated))
			assert.Nil(t, meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed))
			if tc.wantReason != "" {
				ready := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
				require.NotNil(t, ready)
				assert.Equal(t, tc.wantReason, ready.Reason)
				assert.Nil(t, updated.Status.BoundPodSnapshotContentName)
				return
			}
			require.NotNil(t, updated.Status.BoundPodSnapshotContentName)
			content := &snapshotv1alpha1.PodSnapshotContent{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: *updated.Status.BoundPodSnapshotContentName}, content))
			assert.Equal(t, tc.live.UID, content.Spec.Source.PodRef.UID)
			assert.Equal(t, tc.live.Spec.NodeName, content.Spec.Source.NodeName)
		})
	}
}

// A pod that the cache never held produces no pod event when it goes away: the pod watch can list
// again after a gap and skip it. Each case reconciles once while the pod exists, changes the API
// server only, and reconciles again as the backstop requeue does. The PodSnapshot must then reach
// its terminal failure, whatever the pod was when the first pass read it.
func TestSnapshotReconciler_SourcePodGoneBeforeCacheHeldItFails(t *testing.T) {
	s := snapshotReconcilerScheme()
	for _, tc := range []struct {
		name        string
		pinnedUID   types.UID
		cached      *corev1.Pod
		live        *corev1.Pod
		replacement *corev1.Pod
		wantReason  string
	}{
		{
			name:       "unscheduled pod is deleted",
			live:       sourcePodWithUID("pod-uid-9", ""),
			wantReason: "SourcePodNotFound",
		},
		{
			name:       "scheduled pod is deleted",
			live:       sourcePodWithUID("pod-uid-9", "node-a"),
			wantReason: "SourcePodNotFound",
		},
		{
			name:       "pinned pod is deleted and the cache still holds the pod it replaced",
			pinnedUID:  "pod-uid-9",
			cached:     sourcePodWithUID("pod-uid-8", "node-a"),
			live:       sourcePodWithUID("pod-uid-9", "node-a"),
			wantReason: "SourcePodNotFound",
		},
		{
			name:        "pinned pod is replaced by another pod of its name",
			pinnedUID:   "pod-uid-9",
			live:        sourcePodWithUID("pod-uid-9", "node-a"),
			replacement: sourcePodWithUID("pod-uid-10", "node-a"),
			wantReason:  "StalePodReference",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			snap := makeSnapshotForReconcile()
			snap.Spec.Source.PodRef.UID = tc.pinnedUID
			r, apiServer := makeSnapshotReconcilerBehindAPIServer(s, snap, tc.cached, tc.live)

			requireWaitsForCache(t, r, snap)

			require.NoError(t, apiServer.Delete(ctx, tc.live.DeepCopy()))
			if tc.replacement != nil {
				require.NoError(t, apiServer.Create(ctx, tc.replacement.DeepCopy()))
			}
			res := reconcileSnapshot(t, r, snap.Name)
			assert.Zero(t, res.RequeueAfter)

			requireFailedWithReason(t, r, snap, tc.wantReason)
			var contents snapshotv1alpha1.PodSnapshotContentList
			require.NoError(t, r.List(ctx, &contents))
			assert.Empty(t, contents.Items)
		})
	}
}

// A PodSnapshot that pins no UID names whatever pod has the name. When that pod is replaced
// before the cache held it, the PodSnapshot keeps waiting for the cache and binds the pod that
// is there.
func TestSnapshotReconciler_UnpinnedSourcePodReplacedBeforeCacheHeldItBinds(t *testing.T) {
	ctx := context.Background()
	s := snapshotReconcilerScheme()
	snap := makeSnapshotForReconcile()
	first := sourcePodWithUID("pod-uid-9", "node-a")
	r, apiServer := makeSnapshotReconcilerBehindAPIServer(s, snap, nil, first)

	requireWaitsForCache(t, r, snap)
	require.NoError(t, apiServer.Delete(ctx, first.DeepCopy()))
	require.NoError(t, apiServer.Create(ctx, sourcePodWithUID("pod-uid-10", "node-b")))
	requireWaitsForCache(t, r, snap)

	deliverSourcePodToCache(t, r, apiServer)
	reconcileSnapshot(t, r, snap.Name)

	updated := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), updated))
	require.NotNil(t, updated.Status.BoundPodSnapshotContentName)
	content := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: *updated.Status.BoundPodSnapshotContentName}, content))
	assert.Equal(t, types.UID("pod-uid-10"), content.Spec.Source.PodRef.UID)
	assert.Equal(t, "node-b", content.Spec.Source.NodeName)
}

// When the cache and the API server disagree and the API server confirms a failure, the failure
// names what the API server holds, not what the cache holds.
func TestSnapshotReconciler_SourcePodFailureIsDecidedOnTheAPIServerPod(t *testing.T) {
	s := snapshotReconcilerScheme()
	for _, tc := range []struct {
		name        string
		cached      *corev1.Pod
		live        *corev1.Pod
		wantReason  string
		wantMessage string
	}{
		{
			name:        "cache holds another pod and the API server holds none",
			cached:      sourcePodWithUID("pod-uid-8", "node-a"),
			wantReason:  "SourcePodNotFound",
			wantMessage: `source pod "worker-0" no longer exists`,
		},
		{
			name:        "cache holds another pod and the API server holds a third",
			cached:      sourcePodWithUID("pod-uid-8", "node-a"),
			live:        sourcePodWithUID("pod-uid-10", "node-a"),
			wantReason:  "StalePodReference",
			wantMessage: `UID "pod-uid-10", want "pod-uid-9"`,
		},
		{
			name:        "cache holds no pod and the API server holds another pod",
			live:        sourcePodWithUID("pod-uid-10", ""),
			wantReason:  "StalePodReference",
			wantMessage: `UID "pod-uid-10", want "pod-uid-9"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := makeSnapshotForReconcile()
			snap.Spec.Source.PodRef.UID = "pod-uid-9"
			r, _ := makeSnapshotReconcilerBehindAPIServer(s, snap, tc.cached, tc.live)

			res := reconcileSnapshot(t, r, snap.Name)
			assert.Zero(t, res.RequeueAfter)

			updated := requireFailedWithReason(t, r, snap, tc.wantReason)
			failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
			assert.Contains(t, failed.Message, tc.wantMessage)
		})
	}
}

// An API server that cannot answer is not an answer. The error is returned, so the controller
// retries with backoff, and the PodSnapshot is not failed on the word of the cache.
func TestSnapshotReconciler_SourcePodAPIReadErrorIsRetried(t *testing.T) {
	s := snapshotReconcilerScheme()
	snap := makeSnapshotForReconcile()
	r := makeSnapshotReconciler(s, snap) // source pod is absent from the cache
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "worker-0", assert.AnError)
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return forbidden
		},
	}).Build()

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
	require.ErrorIs(t, err, forbidden)
	assert.Zero(t, res.RequeueAfter)

	updated := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), updated))
	assert.Empty(t, updated.Status.Conditions)
	assert.Nil(t, updated.Status.BoundPodSnapshotContentName)
}

// A source pod the cache holds, with the pinned UID when one is pinned, costs no API-server read
// and no requeue: the pod watch delivers every later change of that pod.
func TestSnapshotReconciler_CachedSourcePodNeedsNoAPIRead(t *testing.T) {
	s := snapshotReconcilerScheme()
	for _, tc := range []struct {
		name      string
		pinnedUID types.UID
		cached    *corev1.Pod
	}{
		{name: "scheduled", cached: sourcePodWithUID("pod-uid-9", "node-a")},
		{name: "scheduled and pinned", pinnedUID: "pod-uid-9", cached: sourcePodWithUID("pod-uid-9", "node-a")},
		{name: "unscheduled", cached: sourcePodWithUID("pod-uid-9", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := makeSnapshotForReconcile()
			snap.Spec.Source.PodRef.UID = tc.pinnedUID
			r := makeSnapshotReconciler(s, snap, tc.cached)
			apiReads := 0
			r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					apiReads++
					return assert.AnError
				},
			}).Build()

			res := reconcileSnapshot(t, r, snap.Name)
			assert.Zero(t, res.RequeueAfter)
			assert.Zero(t, apiReads)
		})
	}
}
