// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// ---- buildPodSnapshot / no-ownerReference guarantee ----

func TestBuildPodSnapshot(t *testing.T) {
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-worker-abcde", Namespace: "inference", UID: types.UID("pod-uid")},
	}

	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)

	t.Run("has no ownerReference — the artifact-survival guarantee", func(t *testing.T) {
		assert.Empty(t, snap.OwnerReferences,
			"a controller ownerRef here would make Kubernetes GC delete the artifact when the SnapshotJob is deleted")
	})

	t.Run("carries the owner label instead", func(t *testing.T) {
		assert.Equal(t, sj.Name, snap.Labels[snapshotv1alpha1.SnapshotJobOwnerLabel])
	})

	t.Run("pins the source pod name and UID", func(t *testing.T) {
		assert.Equal(t, pod.Name, snap.Spec.Source.PodRef.Name)
		assert.Equal(t, pod.UID, snap.Spec.Source.PodRef.UID)
	})

	t.Run("carries targetContainers into spec.source.podRef.containers", func(t *testing.T) {
		assert.Equal(t, []string{"worker"}, snap.Spec.Source.PodRef.Containers)
	})

	t.Run("name matches the SnapshotJob's own name", func(t *testing.T) {
		assert.Equal(t, sj.Name, snap.Name)
		assert.Equal(t, sj.Namespace, snap.Namespace)
	})

	t.Run("empty targetContainers is a terminal spec error, not a panic", func(t *testing.T) {
		bad := minimalSnapshotJob()
		bad.Spec.PodSnapshotTemplate.TargetContainers = nil
		_, err := buildPodSnapshot(bad, pod)
		require.Error(t, err)
	})
}

// ---- reconciler-level PodSnapshot creation ----

func TestSnapshotJobReconcileCreatesPodSnapshotForUnscheduledPod(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))

	pod := sourcePodForJob(job)
	// Explicitly unscheduled: no Spec.NodeName. PodSnapshotReconciler (unchanged
	// by this PR) owns waiting for scheduling — this reconciler must still
	// create the PodSnapshot immediately.
	require.Empty(t, pod.Spec.NodeName)

	r := makeSnapshotJobReconciler(s, sj, job, pod)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	snap := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, snap))
	assert.Empty(t, snap.OwnerReferences)
	assert.Equal(t, pod.Name, snap.Spec.Source.PodRef.Name)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.Equal(t, snap.Name, updated.Status.PodSnapshotName)
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonCaptureInProgress, cond.Reason)
}

func TestSnapshotJobReconcileSetsPodPendingWhenPodDoesNotExistYet(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))

	r := makeSnapshotJobReconciler(s, sj, job) // no pod seeded

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	snaps := &snapshotv1alpha1.PodSnapshotList{}
	require.NoError(t, r.List(context.Background(), snaps))
	assert.Empty(t, snaps.Items, "no PodSnapshot until the source pod exists")

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonPodPending, cond.Reason)
}

// ---- Captured mirroring ----

func TestSnapshotJobReconcileCapturedTrueOnPodSnapshotReady(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured",
	})

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonCaptureCompleted, cond.Reason)
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated), "a successful capture must not mark Failed")
}

func TestSnapshotJobReconcileFailedOnPodSnapshotFailed(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue, Reason: "AgentError",
	})

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))

	captured := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, captured)
	assert.Equal(t, metav1.ConditionFalse, captured.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonCaptureFailed, captured.Reason)

	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, metav1.ConditionTrue, failed.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonCaptureFailed, failed.Reason)
	require.NotNil(t, updated.Status.CompletedAt)
}

// ---- AlreadyExists classification (ours = adopt, foreign = conflict, cache-lag = requeue) ----

func TestClassifyExistingPodSnapshot(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "warm-worker-abcde", Namespace: sj.Namespace, UID: types.UID("pod-uid")}}

	t.Run("ours: adopted", func(t *testing.T) {
		owned, err := buildPodSnapshot(sj, pod)
		require.NoError(t, err)
		r := makeSnapshotJobReconciler(s, owned)

		got, err := r.classifyExistingPodSnapshot(context.Background(), sj, owned.Name, errors.New("AlreadyExists"))
		require.NoError(t, err)
		assert.Equal(t, owned.Name, got.Name)
	})

	t.Run("foreign: PodSnapshotNameConflict", func(t *testing.T) {
		foreign := &snapshotv1alpha1.PodSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: sj.Name, Namespace: sj.Namespace},
			Spec: snapshotv1alpha1.PodSnapshotSpec{
				Source: snapshotv1alpha1.PodSnapshotSource{PodRef: snapshotv1alpha1.PodReference{Name: "other-pod", Containers: []string{"main"}}},
			},
		}
		r := makeSnapshotJobReconciler(s, foreign)

		_, err := r.classifyExistingPodSnapshot(context.Background(), sj, foreign.Name, errors.New("AlreadyExists"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, errPodSnapshotNameConflict))
	})

	t.Run("cache lag: NotFound surfaces the original AlreadyExists for requeue", func(t *testing.T) {
		r := makeSnapshotJobReconciler(s) // nothing seeded

		createErr := errors.New("AlreadyExists")
		_, err := r.classifyExistingPodSnapshot(context.Background(), sj, sj.Name, createErr)
		require.Error(t, err)
		assert.ErrorIs(t, err, createErr)
	})
}

// ---- findSourcePod ----

func TestFindSourcePod(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	job.UID = types.UID("job-uid")

	t.Run("not found when no pod exists", func(t *testing.T) {
		r := makeSnapshotJobReconciler(s)
		_, err := findSourcePod(context.Background(), r.Client, job)
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("finds the pod owned by the job, ignoring same-labeled foreign pods", func(t *testing.T) {
		owned := sourcePodForJob(job)
		foreign := sourcePodForJob(job)
		foreign.Name = "someone-elses-pod"
		foreign.OwnerReferences = nil // same job-name label, but not controlled by this Job
		r := makeSnapshotJobReconciler(s, owned, foreign)

		got, err := findSourcePod(context.Background(), r.Client, job)
		require.NoError(t, err)
		assert.Equal(t, owned.Name, got.Name)
	})
}

// ---- mapPodSnapshotToSnapshotJob, including the tombstone ----

func TestMapPodSnapshotToSnapshotJob(t *testing.T) {
	t.Run("maps via the owner label", func(t *testing.T) {
		snap := &snapshotv1alpha1.PodSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Name: "warm-worker", Namespace: "inference",
				Labels: map[string]string{snapshotv1alpha1.SnapshotJobOwnerLabel: "warm-worker"},
			},
		}
		reqs := mapPodSnapshotToSnapshotJob(context.Background(), snap)
		require.Len(t, reqs, 1)
		assert.Equal(t, types.NamespacedName{Namespace: "inference", Name: "warm-worker"}, reqs[0].NamespacedName)
	})

	t.Run("no owner label maps to nothing", func(t *testing.T) {
		snap := &snapshotv1alpha1.PodSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "inference"}}
		assert.Empty(t, mapPodSnapshotToSnapshotJob(context.Background(), snap))
	})

	t.Run("malformed object maps to nothing", func(t *testing.T) {
		assert.Empty(t, mapPodSnapshotToSnapshotJob(context.Background(), &corev1.Pod{}))
	})
}

func TestSnapshotJobOwnerFromPodSnapshotObj(t *testing.T) {
	snap := &snapshotv1alpha1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warm-worker", Namespace: "inference",
			Labels: map[string]string{snapshotv1alpha1.SnapshotJobOwnerLabel: "warm-worker"},
		},
	}

	t.Run("unwraps a delete-event tombstone", func(t *testing.T) {
		tombstone := cache.DeletedFinalStateUnknown{Key: "inference/warm-worker", Obj: snap}
		ref, err := snapshotJobOwnerFromPodSnapshotObj(tombstone)
		require.NoError(t, err)
		assert.Equal(t, types.NamespacedName{Namespace: "inference", Name: "warm-worker"}, ref)
	})

	t.Run("errors on a non-PodSnapshot object", func(t *testing.T) {
		_, err := snapshotJobOwnerFromPodSnapshotObj(&corev1.Pod{})
		require.Error(t, err)
	})
}
