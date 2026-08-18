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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// readySnapshot builds a PodSnapshot for sj/pod with Ready=True.
func readySnapshot(t *testing.T, sj *snapshotv1alpha1.SnapshotJob, pod *corev1.Pod) *snapshotv1alpha1.PodSnapshot {
	t.Helper()
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured",
	})
	return snap
}

// completeJob marks job Complete=True.
func completeJob(job *batchv1.Job) {
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
	})
}

// failJob marks job Failed=True with the given reason.
func failJob(job *batchv1.Job, reason, message string) {
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message,
	})
}

// ---- the two-signal completion gate ----

func TestSnapshotJobReconcileCompletionGate(t *testing.T) {
	t.Run("neither signal: Completed untouched", func(t *testing.T) {
		s := snapshotJobReconcilerScheme()
		sj := minimalSnapshotJob()
		sj.UID = types.UID("sj-uid")
		job, err := buildSourceJob(sj)
		require.NoError(t, err)
		require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
		pod := sourcePodForJob(job)
		snap, err := buildPodSnapshot(sj, pod) // not Ready, not Failed
		require.NoError(t, err)

		r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
		_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
		require.NoError(t, err)

		updated := &snapshotv1alpha1.SnapshotJob{}
		require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
		assert.Nil(t, meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted))
	})

	t.Run("Job complete only: Completed stays False without WaitingForPodCompletion", func(t *testing.T) {
		s := snapshotJobReconcilerScheme()
		sj := minimalSnapshotJob()
		sj.UID = types.UID("sj-uid")
		job, err := buildSourceJob(sj)
		require.NoError(t, err)
		require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
		completeJob(job)
		pod := sourcePodForJob(job)
		snap, err := buildPodSnapshot(sj, pod) // not Ready yet
		require.NoError(t, err)

		r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
		_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
		require.NoError(t, err)

		updated := &snapshotv1alpha1.SnapshotJob{}
		require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
		if cond != nil {
			assert.NotEqual(t, snapshotv1alpha1.ReasonWaitingForPodCompletion, cond.Reason,
				"the dump, not the helpers, is what's pending when the Job finishes before the dump is observed")
		}
		assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))

		jobs := &batchv1.JobList{}
		require.NoError(t, r.List(context.Background(), jobs))
		assert.NotNil(t, getBatchJobByName(jobs, sj.Name), "must not delete the Job before Captured is also True")
	})

	t.Run("Captured only: Completed=False/WaitingForPodCompletion, Job not deleted", func(t *testing.T) {
		s := snapshotJobReconcilerScheme()
		sj := minimalSnapshotJob()
		sj.UID = types.UID("sj-uid")
		job, err := buildSourceJob(sj)
		require.NoError(t, err)
		require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
		pod := sourcePodForJob(job)
		snap := readySnapshot(t, sj, pod)

		r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
		_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
		require.NoError(t, err)

		updated := &snapshotv1alpha1.SnapshotJob{}
		require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
		assert.True(t, meta.IsStatusConditionTrue(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured))
		cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, snapshotv1alpha1.ReasonWaitingForPodCompletion, cond.Reason)

		jobs := &batchv1.JobList{}
		require.NoError(t, r.List(context.Background(), jobs))
		assert.NotNil(t, getBatchJobByName(jobs, sj.Name), "Job must be preserved while helpers are still running")
	})

	t.Run("both signals: Completed=True/JobCompleted, completedAt set, Job deleted", func(t *testing.T) {
		s := snapshotJobReconcilerScheme()
		sj := minimalSnapshotJob()
		sj.UID = types.UID("sj-uid")
		job, err := buildSourceJob(sj)
		require.NoError(t, err)
		require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
		completeJob(job)
		pod := sourcePodForJob(job)
		snap := readySnapshot(t, sj, pod)

		r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
		_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
		require.NoError(t, err)

		updated := &snapshotv1alpha1.SnapshotJob{}
		require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
		assert.True(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
		cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
		require.NotNil(t, cond)
		assert.Equal(t, snapshotv1alpha1.ReasonJobCompleted, cond.Reason)
		require.NotNil(t, updated.Status.CompletedAt)

		jobs := &batchv1.JobList{}
		require.NoError(t, r.List(context.Background(), jobs))
		assert.Nil(t, getBatchJobByName(jobs, sj.Name), "Job must be deleted on success")

		snaps := &snapshotv1alpha1.PodSnapshotList{}
		require.NoError(t, r.List(context.Background(), snaps))
		require.Len(t, snaps.Items, 1, "the PodSnapshot must survive SnapshotJob completion")
	})
}

// ---- Job-failure-wins-over-Captured ----

func TestSnapshotJobReconcileJobFailureWinsOverCaptured(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	snap := readySnapshot(t, sj, pod)

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	require.True(t, meta.IsStatusConditionTrue(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured),
		"precondition: capture must have succeeded before the Job fails")

	// A helper container now fails after the dump already succeeded.
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, job))
	failJob(job, "BackoffLimitExceeded", "container helper exited 1")
	require.NoError(t, r.Status().Update(context.Background(), job))

	_, err = r.Reconcile(context.Background(), reconcileRequest(updated))
	require.NoError(t, err)

	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.True(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonJobFailed, failed.Reason)
	assert.True(t, meta.IsStatusConditionTrue(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured),
		"Captured=True is not reverted by a later Failed=True — it records that the CRIU artifact exists")

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.NotNil(t, getBatchJobByName(jobs, sj.Name), "the Job must be preserved on Failed=True, even after a successful capture")
}

func TestSnapshotJobReconcileDeadlineExceeded(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	failJob(job, batchv1.JobReasonDeadlineExceeded, "Job was active longer than specified deadline")

	r := makeSnapshotJobReconciler(s, sj, job)
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, snapshotv1alpha1.ReasonDeadlineExceeded, cond.Reason)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.NotNil(t, getBatchJobByName(jobs, sj.Name), "preserved for debug")
}

// ---- the remaining non-execution failure reasons ----

func TestSnapshotJobReconcileJobDeleted(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	sj.Status.PodSnapshotName = sj.Name // "already progressed" — the Job existed at least this long

	r := makeSnapshotJobReconciler(s, sj) // no Job seeded: it has vanished
	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, snapshotv1alpha1.ReasonJobDeleted, cond.Reason)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.Empty(t, jobs.Items, "nothing to preserve — it was already gone")
}

func TestSnapshotJobReconcilePodSnapshotNameConflict(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)

	foreign := &snapshotv1alpha1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: sj.Name, Namespace: sj.Namespace}, // no owner label
		Spec: snapshotv1alpha1.PodSnapshotSpec{
			Source: snapshotv1alpha1.PodSnapshotSource{PodRef: snapshotv1alpha1.PodReference{Name: "other-pod", Containers: []string{"main"}}},
		},
	}

	r := makeSnapshotJobReconciler(s, sj, job, pod, foreign)
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, snapshotv1alpha1.ReasonPodSnapshotNameConflict, cond.Reason)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.NotNil(t, getBatchJobByName(jobs, sj.Name), "preserved: the Job existed before the conflicting PodSnapshot was found")
}

// ---- PodSnapshotFailed vs CaptureFailed classification ----

func TestCaptureFailureReason(t *testing.T) {
	cases := []struct {
		name       string
		condReason string
		want       string
	}{
		{"bind-stage: ContentConflict maps to PodSnapshotFailed", "ContentConflict", snapshotv1alpha1.ReasonPodSnapshotFailed},
		{"bind-stage: SourcePodNotFound maps to PodSnapshotFailed", "SourcePodNotFound", snapshotv1alpha1.ReasonPodSnapshotFailed},
		{"bind-stage: StalePodReference maps to PodSnapshotFailed", "StalePodReference", snapshotv1alpha1.ReasonPodSnapshotFailed},
		{"agent-mirrored reason maps to CaptureFailed", "CRIUDumpFailed", snapshotv1alpha1.ReasonCaptureFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &snapshotv1alpha1.PodSnapshot{}
			meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
				Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue, Reason: tc.condReason,
			})
			reason, _ := captureFailureReason(snap)
			assert.Equal(t, tc.want, reason)
		})
	}
}

func TestSnapshotJobReconcilePodSnapshotFailedReasonIsBindStage(t *testing.T) {
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
		Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue, Reason: "ContentConflict",
	})

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, snapshotv1alpha1.ReasonPodSnapshotFailed, cond.Reason)
}

// ---- terminal re-entry and cleanup retry ----

func TestSnapshotJobReconcileCompletedRetriesCleanupUntilJobGone(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	now := metav1.Now()
	sj.Status.CompletedAt = &now
	meta.SetStatusCondition(&sj.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.SnapshotJobConditionCompleted, Status: metav1.ConditionTrue, Reason: snapshotv1alpha1.ReasonJobCompleted,
	})
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))

	r := makeSnapshotJobReconciler(s, sj, job)

	// First pass: status is already terminal, but the Job is still present —
	// cleanup must still run rather than short-circuiting like Failed does.
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.Nil(t, getBatchJobByName(jobs, sj.Name), "cleanup must run for a Completed SnapshotJob whose Job wasn't deleted yet")

	// Second pass: Job already gone — a pure no-op, not an error.
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
}

func TestSnapshotJobReconcileFailedNeverDeletesTheJob(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	meta.SetStatusCondition(&sj.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.SnapshotJobConditionFailed, Status: metav1.ConditionTrue, Reason: snapshotv1alpha1.ReasonJobFailed,
	})
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))

	r := makeSnapshotJobReconciler(s, sj, job)
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.NotNil(t, getBatchJobByName(jobs, sj.Name), "Failed short-circuits entirely — cleanup never runs")
}

// ---- ordering: the terminal status write must persist before the Job delete is attempted ----

func TestSnapshotJobReconcileCompletionPersistsStatusBeforeDeleting(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	completeJob(job)
	pod := sourcePodForJob(job)
	snap := readySnapshot(t, sj, pod)

	funcs := interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			return apierrors.NewInternalError(assertAnError{})
		}
		return c.Delete(ctx, obj, opts...)
	}}
	r := makeSnapshotJobReconcilerWithInterceptor(s, funcs, sj, job, pod, snap)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.Error(t, err, "a failed Delete must be retried, not swallowed")

	// Despite the Delete failure, the terminal status write must already have
	// landed — otherwise a crash here would lose the record of why the Job is
	// (about to be) gone.
	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.True(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
	require.NotNil(t, updated.Status.CompletedAt)
}

type assertAnError struct{}

func (assertAnError) Error() string { return "delete failed" }
