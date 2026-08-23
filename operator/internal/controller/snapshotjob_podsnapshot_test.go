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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
		assert.Equal(t, string(sj.UID), snap.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel])
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

	t.Run("more than one targetContainers entry is a terminal spec error", func(t *testing.T) {
		// buildSourceJob rejects this on the create path, but a Job that already
		// exists (the adopt/observe paths) skips buildSourceJob entirely — this is
		// the only other place that reads targetContainers, so it must enforce the
		// same exactly-one constraint independently.
		bad := minimalSnapshotJob()
		bad.Spec.PodSnapshotTemplate.TargetContainers = []string{"worker", "helper"}
		_, err := buildPodSnapshot(bad, pod)
		require.Error(t, err)
	})

	t.Run("does not share a backing array with the SnapshotJob's own slice", func(t *testing.T) {
		src := minimalSnapshotJob()
		original := append([]string(nil), src.Spec.PodSnapshotTemplate.TargetContainers...)

		got, err := buildPodSnapshot(src, pod)
		require.NoError(t, err)
		got.Spec.Source.PodRef.Containers[0] = "mutated"

		assert.Equal(t, original, src.Spec.PodSnapshotTemplate.TargetContainers,
			"mutating the PodSnapshot's copy must not affect the SnapshotJob's own spec slice")
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

	result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, sourcePodRequeueBackstop, result.RequeueAfter)

	snaps := &snapshotv1alpha1.PodSnapshotList{}
	require.NoError(t, r.List(context.Background(), snaps))
	assert.Empty(t, snaps.Items, "no PodSnapshot until the source pod exists")

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonPodPending, cond.Reason)
	assert.Equal(t, "waiting for the source Job to create a pod", cond.Message)
}

// TestSnapshotJobReconcileObserveRecoversPodSnapshotIdentity covers a status-write
// conflict after PodSnapshot creation. The next reconcile must reconstruct the
// name and UID from the observed resource.
func TestSnapshotJobReconcileObserveRecoversPodSnapshotIdentity(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	snap.UID = types.UID("pod-snapshot-uid")

	// sj.Status.PodSnapshotName is deliberately left empty, simulating a lost
	// write, even though the PodSnapshot already exists.
	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.Equal(t, snap.Name, updated.Status.PodSnapshotName,
		"status derivation must recover the PodSnapshot name")
	assert.Equal(t, snap.UID, updated.Status.PodSnapshotUID,
		"status derivation must recover the PodSnapshot UID")
}

func TestSnapshotJobReconcileRecoversPendingCapturedCondition(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	meta.SetStatusCondition(&sj.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.SnapshotJobConditionCaptured, Status: metav1.ConditionTrue,
		Reason: snapshotv1alpha1.ReasonCaptureCompleted, Message: "stale status",
	})

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod) // no terminal condition: capture is pending
	require.NoError(t, err)
	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	captured := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, captured)
	assert.Equal(t, metav1.ConditionFalse, captured.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonCaptureInProgress, captured.Reason)
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

// TestSnapshotJobReconcileFailureTargetWinsOverCaptureFailure verifies the
// condition Kubernetes publishes before terminal Failed=True is sufficient to
// classify a raced deadline expiry.
func TestSnapshotJobReconcileFailureTargetWinsOverCaptureFailure(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue,
		Reason: batchv1.JobReasonDeadlineExceeded, Message: "Job was active longer than specified deadline",
	})
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue,
		Reason: "SourcePodGone", Message: `source pod "x" is no longer running (phase Running)`,
	})

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)

	res, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonDeadlineExceeded, failed.Reason,
		"FailureTarget/DeadlineExceeded must win over the raced CaptureFailed")
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

		got, err := r.classifyExistingPodSnapshot(context.Background(), sj, owned, errors.New("AlreadyExists"))
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
		desired, err := buildPodSnapshot(sj, pod)
		require.NoError(t, err)

		_, err = r.classifyExistingPodSnapshot(context.Background(), sj, desired, errors.New("AlreadyExists"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, errPodSnapshotNameConflict))
	})

	t.Run("stale SnapshotJob UID: PodSnapshotNameConflict", func(t *testing.T) {
		stale, err := buildPodSnapshot(sj, pod)
		require.NoError(t, err)
		stale.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] = "old-sj-uid"
		r := makeSnapshotJobReconciler(s, stale)
		desired, err := buildPodSnapshot(sj, pod)
		require.NoError(t, err)

		_, err = r.classifyExistingPodSnapshot(context.Background(), sj, desired, errors.New("AlreadyExists"))
		require.Error(t, err)
		assert.ErrorIs(t, err, errPodSnapshotNameConflict)
	})

	t.Run("lookup rejects a stale SnapshotJob UID", func(t *testing.T) {
		stale, err := buildPodSnapshot(sj, pod)
		require.NoError(t, err)
		stale.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] = "old-sj-uid"
		r := makeSnapshotJobReconciler(s, stale)

		_, err = r.findOwnedPodSnapshot(context.Background(), sj)
		require.Error(t, err)
		assert.ErrorIs(t, err, errPodSnapshotNameConflict)
	})

	t.Run("lookup ignores a differently named PodSnapshot with the same owner labels", func(t *testing.T) {
		unrelated, err := buildPodSnapshot(sj, pod)
		require.NoError(t, err)
		unrelated.Name = "unrelated-capture"
		r := makeSnapshotJobReconciler(s, unrelated)

		_, err = r.findOwnedPodSnapshot(context.Background(), sj)
		require.Error(t, err)
		assert.True(t, apierrors.IsNotFound(err))
	})

	t.Run("lookup returns the deterministic artifact alongside an unrelated labeled object", func(t *testing.T) {
		owned, err := buildPodSnapshot(sj, pod)
		require.NoError(t, err)
		unrelated := owned.DeepCopy()
		unrelated.Name = "unrelated-capture"
		r := makeSnapshotJobReconciler(s, owned, unrelated)

		got, err := r.findOwnedPodSnapshot(context.Background(), sj)
		require.NoError(t, err)
		assert.Equal(t, owned.Name, got.Name)
	})

	t.Run("lookup rejects a same-name replacement UID", func(t *testing.T) {
		tracked := sj.DeepCopy()
		tracked.Status.PodSnapshotUID = types.UID("original-snapshot-uid")
		replacement, err := buildPodSnapshot(tracked, pod)
		require.NoError(t, err)
		replacement.UID = types.UID("replacement-snapshot-uid")
		r := makeSnapshotJobReconciler(s, replacement)

		_, err = r.findOwnedPodSnapshot(context.Background(), tracked)
		require.Error(t, err)
		assert.ErrorIs(t, err, errPodSnapshotNameConflict)
	})

	t.Run("AlreadyExists rejects owned PodSnapshot with different source identity", func(t *testing.T) {
		desired, err := buildPodSnapshot(sj, pod)
		require.NoError(t, err)
		existing := desired.DeepCopy()
		existing.Spec.Source.PodRef.UID = types.UID("different-pod-uid")
		r := makeSnapshotJobReconciler(s, existing)

		_, err = r.classifyExistingPodSnapshot(context.Background(), sj, desired, errors.New("AlreadyExists"))
		require.Error(t, err)
		assert.ErrorIs(t, err, errPodSnapshotNameConflict)
	})

	t.Run("cache lag: NotFound surfaces the original AlreadyExists for requeue", func(t *testing.T) {
		r := makeSnapshotJobReconciler(s) // nothing seeded
		desired, err := buildPodSnapshot(sj, pod)
		require.NoError(t, err)

		createErr := errors.New("AlreadyExists")
		_, err = r.classifyExistingPodSnapshot(context.Background(), sj, desired, createErr)
		require.Error(t, err)
		assert.ErrorIs(t, err, createErr)
	})
}

func TestSnapshotJobReconcileRecordedPodSnapshotMissingFailsWithoutRecreation(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.Status.SourceJobUID = types.UID("source-job-uid")
	sj.Status.PodSnapshotName = sj.Name
	sj.Status.PodSnapshotUID = types.UID("original-snapshot-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	job.UID = sj.Status.SourceJobUID
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	r := makeSnapshotJobReconciler(s, sj, job) // the recorded PodSnapshot is gone

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonPodSnapshotDeleted, failed.Reason)
	assert.Equal(t, types.UID("original-snapshot-uid"), updated.Status.PodSnapshotUID)

	snaps := &snapshotv1alpha1.PodSnapshotList{}
	require.NoError(t, r.List(context.Background(), snaps))
	assert.Empty(t, snaps.Items, "a one-shot capture must not be recreated after its recorded PodSnapshot disappears")
}

func TestSnapshotJobReconcileRecordedPodSnapshotCacheMissUsesAuthoritativeReader(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.Status.SourceJobUID = types.UID("source-job-uid")
	sj.Status.PodSnapshotName = sj.Name
	sj.Status.PodSnapshotUID = types.UID("pod-snapshot-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	job.UID = sj.Status.SourceJobUID
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	snap.UID = sj.Status.PodSnapshotUID

	// The cached client does not contain the PodSnapshot, simulating informer lag.
	// The authoritative reader does, so the recorded child must remain valid.
	r := makeSnapshotJobReconciler(s, sj, job, pod)
	r.APIReader = fake.NewClientBuilder().WithScheme(s).WithObjects(snap).Build()

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated),
		"a stale cached NotFound must not become permanent PodSnapshotDeleted")
	assert.Equal(t, snap.UID, updated.Status.PodSnapshotUID)
}

func TestSnapshotJobReconcileRecordedPodSnapshotAuthoritativeReadErrorRetries(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.Status.SourceJobUID = types.UID("source-job-uid")
	sj.Status.PodSnapshotName = sj.Name
	sj.Status.PodSnapshotUID = types.UID("pod-snapshot-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	job.UID = sj.Status.SourceJobUID
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	r := makeSnapshotJobReconciler(s, sj, job) // cached PodSnapshot lookup misses
	r.APIReader = fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("transient API read failure")
		},
	}).Build()

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.Error(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated),
		"an incomplete authoritative observation must be retried, not persisted as terminal")
}

func TestSnapshotJobReconcileRejectsSameNamePodSnapshotReplacement(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.Status.SourceJobUID = types.UID("source-job-uid")
	sj.Status.PodSnapshotName = sj.Name
	sj.Status.PodSnapshotUID = types.UID("original-snapshot-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	job.UID = sj.Status.SourceJobUID
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	replacement, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	replacement.UID = types.UID("replacement-snapshot-uid")
	r := makeSnapshotJobReconciler(s, sj, job, pod, replacement)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonPodSnapshotNameConflict, failed.Reason)
	assert.Equal(t, types.UID("original-snapshot-uid"), updated.Status.PodSnapshotUID,
		"a replacement UID must never overwrite the recorded capture identity")
}

func TestSnapshotJobReconcileRejectsPodSnapshotSpecDriftBeforeUIDBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*snapshotv1alpha1.PodSnapshot)
	}{
		{
			name: "source pod name",
			mutate: func(snap *snapshotv1alpha1.PodSnapshot) {
				snap.Spec.Source.PodRef.Name = "other-pod"
			},
		},
		{
			name: "source pod UID",
			mutate: func(snap *snapshotv1alpha1.PodSnapshot) {
				snap.Spec.Source.PodRef.UID = types.UID("other-pod-uid")
			},
		},
		{
			name: "target containers",
			mutate: func(snap *snapshotv1alpha1.PodSnapshot) {
				snap.Spec.Source.PodRef.Containers = []string{"other-container"}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := snapshotJobReconcilerScheme()
			sj := minimalSnapshotJob()
			sj.Status.SourceJobUID = types.UID("source-job-uid")
			job, err := buildSourceJob(sj)
			require.NoError(t, err)
			job.UID = sj.Status.SourceJobUID
			require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
			pod := sourcePodForJob(job)
			snap, err := buildPodSnapshot(sj, pod)
			require.NoError(t, err)
			snap.UID = types.UID("pod-snapshot-uid")
			test.mutate(snap)
			r := makeSnapshotJobReconciler(s, sj, job, pod, snap)

			_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
			require.NoError(t, err)

			updated := &snapshotv1alpha1.SnapshotJob{}
			require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
			failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, snapshotv1alpha1.ReasonPodSnapshotNameConflict, failed.Reason)
			assert.Empty(t, updated.Status.PodSnapshotUID,
				"an unverified PodSnapshot UID must not be persisted")
		})
	}
}

// ---- findSourcePod ----

func TestFindSourcePod(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	job.UID = types.UID("job-uid")

	t.Run("reports pending domain state when no pod exists", func(t *testing.T) {
		r := makeSnapshotJobReconciler(s)
		pod, found, err := findSourcePod(context.Background(), r.Client, job)
		require.NoError(t, err)
		assert.False(t, found)
		assert.Nil(t, pod)
	})

	t.Run("finds the pod owned by the job, ignoring same-labeled foreign pods", func(t *testing.T) {
		owned := sourcePodForJob(job)
		foreign := sourcePodForJob(job)
		foreign.Name = "someone-elses-pod"
		foreign.OwnerReferences = nil // same job-name label, but not controlled by this Job
		r := makeSnapshotJobReconciler(s, owned, foreign)

		got, found, err := findSourcePod(context.Background(), r.Client, job)
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, owned.Name, got.Name)
	})

	t.Run("errors when more than one pod is controlled by the job", func(t *testing.T) {
		first := sourcePodForJob(job)
		second := sourcePodForJob(job)
		second.Name = "warm-worker-fghij"
		r := makeSnapshotJobReconciler(s, first, second)

		pod, found, err := findSourcePod(context.Background(), r.Client, job)
		require.Error(t, err)
		assert.False(t, found)
		assert.Nil(t, pod)
		assert.Contains(t, err.Error(), "controls 2 pods")
	})

	t.Run("returns list errors as retryable API failures", func(t *testing.T) {
		listErr := errors.New("transient list failure")
		r := makeSnapshotJobReconcilerWithInterceptor(s, interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return listErr
			},
		})

		pod, found, err := findSourcePod(context.Background(), r.Client, job)
		assert.ErrorIs(t, err, listErr)
		assert.False(t, found)
		assert.Nil(t, pod)
	})
}

func TestSnapshotJobReconcileUsesAPIReaderForSourcePod(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)

	r := makeSnapshotJobReconciler(s, sj, job) // cached client deliberately lacks the pod
	r.APIReader = fake.NewClientBuilder().WithScheme(s).WithObjects(pod).Build()

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	snap := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, snap))
}

// ---- mapPodSnapshotToSnapshotJob ----

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

func TestSnapshotJobOwnerFromPodSnapshotObjRejectsWrongType(t *testing.T) {
	_, err := snapshotJobOwnerFromPodSnapshotObj(&corev1.Pod{})
	require.Error(t, err)
}
