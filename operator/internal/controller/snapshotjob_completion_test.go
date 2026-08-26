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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

func setJobFailureCondition(job *batchv1.Job, conditionType batchv1.JobConditionType, reason, message string) {
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type: conditionType, Status: corev1.ConditionTrue, Reason: reason, Message: message,
	})
}

func TestSnapshotJobTerminalFailure(t *testing.T) {
	ready := &snapshotv1alpha1.PodSnapshot{Status: snapshotv1alpha1.PodSnapshotStatus{Conditions: []metav1.Condition{{
		Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured",
	}}}}
	failed := &snapshotv1alpha1.PodSnapshot{Status: snapshotv1alpha1.PodSnapshotStatus{Conditions: []metav1.Condition{{
		Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue, Reason: "CRIUDumpFailed", Message: "dump failed",
	}}}}
	sourceCompletedWithoutCapture := &snapshotv1alpha1.PodSnapshot{Status: snapshotv1alpha1.PodSnapshotStatus{Conditions: []metav1.Condition{{
		Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue,
		Reason: snapshotv1alpha1.ReasonSourceCompletedWithoutCapture, Message: "source exited without a capture result",
	}}}}
	pending := &snapshotv1alpha1.PodSnapshot{}

	complete := func() batchv1.JobCondition {
		return batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
	}
	failedCondition := func(conditionType batchv1.JobConditionType, reason string) batchv1.JobCondition {
		return batchv1.JobCondition{Type: conditionType, Status: corev1.ConditionTrue, Reason: reason, Message: "source stopped"}
	}

	tests := []struct {
		name       string
		conditions []batchv1.JobCondition
		snapshot   *snapshotv1alpha1.PodSnapshot
		wantReason string
	}{
		{name: "active Job and pending capture keep waiting", snapshot: pending},
		{name: "Complete=False is not terminal", conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}}, snapshot: pending},
		{name: "completed Job and pending capture wait for a source or capture event", conditions: []batchv1.JobCondition{complete()}, snapshot: pending},
		{name: "completed Job and missing PodSnapshot wait for resource reconciliation", conditions: []batchv1.JobCondition{complete()}},
		{name: "FailureTarget with no capture fails immediately", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailureTarget, "BackoffLimitExceeded")}, wantReason: snapshotv1alpha1.ReasonJobFailed},
		{name: "Failed with no capture fails immediately", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailed, "BackoffLimitExceeded")}, wantReason: snapshotv1alpha1.ReasonJobFailed},
		{name: "deadline with no capture fails immediately", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailureTarget, batchv1.JobReasonDeadlineExceeded)}, wantReason: snapshotv1alpha1.ReasonDeadlineExceeded},
		{name: "deadline takes precedence over generic failure", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailed, "BackoffLimitExceeded"), failedCondition(batchv1.JobFailureTarget, batchv1.JobReasonDeadlineExceeded)}, wantReason: snapshotv1alpha1.ReasonDeadlineExceeded},
		{name: "FailureTarget and pending capture wait for the capture result", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailureTarget, "BackoffLimitExceeded")}, snapshot: pending},
		{name: "Failed and pending capture wait for the capture result", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailed, "BackoffLimitExceeded")}, snapshot: pending},
		{name: "deadline and pending capture wait for the capture result", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailureTarget, batchv1.JobReasonDeadlineExceeded)}, snapshot: pending},
		{name: "completed Job and ready capture are successful signals", conditions: []batchv1.JobCondition{complete()}, snapshot: ready},
		{name: "active Job and ready capture are successful signals", snapshot: ready},
		{name: "ready capture wins over source Job failure", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailed, "BackoffLimitExceeded")}, snapshot: ready},
		{name: "ready capture wins over the source Job deadline", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailureTarget, batchv1.JobReasonDeadlineExceeded)}, snapshot: ready},
		{name: "PodSnapshot failure reports capture failure", snapshot: failed, wantReason: snapshotv1alpha1.ReasonCaptureFailed},
		{name: "source completion without capture preserves its specific reason", conditions: []batchv1.JobCondition{complete()}, snapshot: sourceCompletedWithoutCapture, wantReason: snapshotv1alpha1.ReasonSourceCompletedWithoutCapture},
		{name: "capture failure detail wins over a raced Job failure", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailed, "BackoffLimitExceeded")}, snapshot: failed, wantReason: snapshotv1alpha1.ReasonCaptureFailed},
		{name: "deadline expiry wins over its collateral capture failure", conditions: []batchv1.JobCondition{failedCondition(batchv1.JobFailureTarget, batchv1.JobReasonDeadlineExceeded)}, snapshot: failed, wantReason: snapshotv1alpha1.ReasonDeadlineExceeded},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: test.conditions}}
			failure := snapshotJobTerminalFailure(job, test.snapshot)
			if test.wantReason == "" {
				assert.Nil(t, failure)
				return
			}
			require.NotNil(t, failure)
			assert.Equal(t, test.wantReason, failure.reason)
			assert.NotNil(t, failure.cause)
		})
	}
}

// ---- condition lifecycle: present from the start, append-only ----

func TestSnapshotJobConditionsPresentFromFirstReconcileAndNeverRemoved(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	r := makeSnapshotJobReconciler(s, sj)

	// First reconcile creates the source Job and must already write all four
	// conditions, so a consumer can always distinguish "known False" from
	// "not yet evaluated".
	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	want := map[string]string{
		snapshotv1alpha1.SnapshotJobConditionRunning:   snapshotv1alpha1.ReasonPodPending,
		snapshotv1alpha1.SnapshotJobConditionCaptured:  snapshotv1alpha1.ReasonCaptureInProgress,
		snapshotv1alpha1.SnapshotJobConditionCompleted: snapshotv1alpha1.ReasonCaptureInProgress,
		snapshotv1alpha1.SnapshotJobConditionFailed:    snapshotv1alpha1.ReasonNoFailure,
	}
	require.Len(t, updated.Status.Conditions, len(want))
	for conditionType, reason := range want {
		cond := meta.FindStatusCondition(updated.Status.Conditions, conditionType)
		require.NotNil(t, cond, "%s must be present from the first status write", conditionType)
		assert.Equal(t, metav1.ConditionFalse, cond.Status, conditionType)
		assert.Equal(t, reason, cond.Reason, conditionType)
	}

	// Later reconciles only update conditions — none may disappear.
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	require.Len(t, updated.Status.Conditions, len(want), "conditions are append-only, never removed")
	for conditionType := range want {
		require.NotNil(t, meta.FindStatusCondition(updated.Status.Conditions, conditionType), conditionType)
	}
}

// ---- two-signal completion ----

func TestSnapshotJobReconcileCompletionGate(t *testing.T) {
	t.Run("neither signal: all four conditions present and non-terminal", func(t *testing.T) {
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
		completed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
		require.NotNil(t, completed, "all conditions are present from the first status write")
		assert.Equal(t, metav1.ConditionFalse, completed.Status)
		assert.Equal(t, snapshotv1alpha1.ReasonCaptureInProgress, completed.Reason)
		failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
		require.NotNil(t, failed)
		assert.Equal(t, metav1.ConditionFalse, failed.Status)
		assert.Equal(t, snapshotv1alpha1.ReasonNoFailure, failed.Reason)
	})

	t.Run("Job complete only: waits for capture", func(t *testing.T) {
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
		result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
		require.NoError(t, err)
		assert.Equal(t, captureResolutionBackstop, result.RequeueAfter,
			"a terminal source Job with a pending capture must schedule a backstop re-check")

		updated := &snapshotv1alpha1.SnapshotJob{}
		require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
		assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
		completed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
		require.NotNil(t, completed)
		assert.Equal(t, metav1.ConditionFalse, completed.Status)
		assert.Equal(t, snapshotv1alpha1.ReasonCaptureInProgress, completed.Reason,
			"WaitingForPodCompletion applies only after capture has succeeded")

		jobs := &batchv1.JobList{}
		require.NoError(t, r.List(context.Background(), jobs))
		assert.NotNil(t, getBatchJobByName(jobs, sj.Name), "must not delete the Job before Captured is also True")
	})

	t.Run("capture success waits for the source Job to finish before completing", func(t *testing.T) {
		s := snapshotJobReconcilerScheme()
		sj := minimalSnapshotJob()
		sj.UID = types.UID("sj-uid")
		job, err := buildSourceJob(sj)
		require.NoError(t, err)
		require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
		job.UID = types.UID("source-job-uid")
		sj.Status.SourceJobUID = job.UID
		pod := sourcePodForJob(job)
		snap := readySnapshot(t, sj, pod)

		r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
		result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
		require.NoError(t, err)
		assert.Equal(t, captureResolutionBackstop, result.RequeueAfter,
			"a Ready capture with a still-running source Job schedules a backstop re-check")

		updated := &snapshotv1alpha1.SnapshotJob{}
		require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
		assert.True(t, meta.IsStatusConditionTrue(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured))
		cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, snapshotv1alpha1.ReasonWaitingForPodCompletion, cond.Reason,
			"cleanup must wait for helper containers; deleting the Job now would kill them mid-work")
		assert.Nil(t, updated.Status.CompletedAt)

		jobs := &batchv1.JobList{}
		require.NoError(t, r.List(context.Background(), jobs))
		storedJob := getBatchJobByName(jobs, sj.Name)
		require.NotNil(t, storedJob, "Job must remain while its containers are still running")

		// The owned-Job update watch drives this second reconcile in production.
		completeJob(storedJob)
		require.NoError(t, r.Status().Update(context.Background(), storedJob))
		_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
		require.NoError(t, err)

		updated = &snapshotv1alpha1.SnapshotJob{}
		require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
		cond = meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
		require.NotNil(t, cond)
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, snapshotv1alpha1.ReasonJobCompleted, cond.Reason)
		require.NotNil(t, updated.Status.CompletedAt)
		running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
		require.NotNil(t, running)
		assert.Equal(t, metav1.ConditionFalse, running.Status)
		assert.Equal(t, snapshotv1alpha1.ReasonJobCompleted, running.Reason)
		assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))

		jobs = &batchv1.JobList{}
		require.NoError(t, r.List(context.Background(), jobs))
		assert.Nil(t, getBatchJobByName(jobs, sj.Name), "Job is deleted only after capture and source completion")

		snaps := &snapshotv1alpha1.PodSnapshotList{}
		require.NoError(t, r.List(context.Background(), snaps))
		require.Len(t, snaps.Items, 1, "the PodSnapshot must survive SnapshotJob completion")
	})
}

func TestSnapshotJobReconcileCompletedJobWithMissingSourcePodFailsWithoutPolling(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	completeJob(job)

	r := makeSnapshotJobReconciler(s, sj, job)
	result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonSourceCompletedWithoutCapture, failed.Reason)
	assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
}

func TestSnapshotJobReconcileCompletedJobWithRetainedPodFailsWithoutCreatingCapture(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	completeJob(job)
	pod := sourcePodForJob(job)
	pod.Status.Phase = corev1.PodSucceeded

	// A complete source Job without a capture is the same logical state whether
	// or not its succeeded pod is still retained: the containers have exited, so
	// a capture created now can never succeed.
	r := makeSnapshotJobReconciler(s, sj, job, pod)
	result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed, "pod retention timing must not change the outcome of a complete Job without a capture")
	assert.Equal(t, snapshotv1alpha1.ReasonSourceCompletedWithoutCapture, failed.Reason)

	snaps := &snapshotv1alpha1.PodSnapshotList{}
	require.NoError(t, r.List(context.Background(), snaps))
	assert.Empty(t, snaps.Items, "no capture may be created against a source whose containers have exited")
}

func TestSnapshotJobReconcileCompletedJobConfirmsCaptureAbsenceAuthoritatively(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	completeJob(job)
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)

	// The capture was created but the crash window left it unrecorded in status,
	// and the informer has not caught up: only the authoritative reader sees it.
	// The cached miss must not become a SourceCompletedWithoutCapture failure.
	r := makeSnapshotJobReconciler(s, sj, job, pod)
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithObjects(snap, pod).Build()

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated),
		"a capture hidden by informer lag must not be classified as never created")
}

// ---- capture success overrides source Job failure ----

func TestSnapshotJobReconcileCaptureSuccessOverridesJobFailure(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	job.Status.Ready = ptr.To(int32(1))
	pod := sourcePodForJob(job)
	snap := readySnapshot(t, sj, pod)
	setJobFailureCondition(job, batchv1.JobFailureTarget, "BackoffLimitExceeded", "checkpoint terminated the source process")

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.True(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated),
		"the checkpoint terminates the source process, so a failed source Job after a durable capture is the expected success sequence")
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
	assert.True(t, meta.IsStatusConditionTrue(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured))
	completed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
	require.NotNil(t, completed)
	assert.Equal(t, snapshotv1alpha1.ReasonJobCompleted, completed.Reason)
	running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running)
	assert.Equal(t, metav1.ConditionFalse, running.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonJobCompleted, running.Reason,
		"a completed SnapshotJob must not advertise the expected source exit as JobFailed")
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.Nil(t, getBatchJobByName(jobs, sj.Name), "the failed-but-successful source Job is cleaned up on completion")
}

// helperSnapshotJob builds a SnapshotJob whose pod template carries a helper
// container next to the target, plus the matching source pod with the target
// already killed by the checkpoint (exit 137) and the helper in the given state.
func helperSnapshotJob(t *testing.T, s *runtime.Scheme, helperState corev1.ContainerState) (*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod, *snapshotv1alpha1.PodSnapshot) {
	t.Helper()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	sj.Spec.PodTemplate.Spec.Containers = append(sj.Spec.PodTemplate.Spec.Containers,
		corev1.Container{Name: "helper", Image: "test:latest"})
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "worker", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}},
		{Name: "helper", State: helperState},
	}
	snap := readySnapshot(t, sj, pod)
	setJobFailureCondition(job, batchv1.JobFailed, "BackoffLimitExceeded", "checkpoint terminated the target container")
	return sj, job, pod, snap
}

func TestSnapshotJobReconcileHelperExitZeroCompletes(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj, job, pod, snap := helperSnapshotJob(t, s,
		corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.True(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated),
		"a helper that finished its work with exit 0 must not block completion; only the target's kill exit is in the Job failure")
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
}

func TestSnapshotJobReconcileHelperFailureFailsAfterCapture(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj, job, pod, snap := helperSnapshotJob(t, s,
		corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}})

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonJobFailed, failed.Reason,
		"an incomplete helper deliverable fails the SnapshotJob even though the capture succeeded")
	assert.Contains(t, failed.Message, "helper")
	assert.True(t, meta.IsStatusConditionTrue(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured),
		"capture success remains independently visible")

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.NotNil(t, getBatchJobByName(jobs, sj.Name), "failed source Job must be preserved for debugging")
}

func TestSnapshotJobReconcileHelperStillRunningWaits(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	// FailureTarget can land before the helper's final container state.
	sj, job, pod, snap := helperSnapshotJob(t, s,
		corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, captureResolutionBackstop, result.RequeueAfter)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
	completed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
	require.NotNil(t, completed)
	assert.Equal(t, snapshotv1alpha1.ReasonWaitingForPodCompletion, completed.Reason)
}

func TestSnapshotJobReconcileHelperMissingStatusWaits(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj, job, pod, snap := helperSnapshotJob(t, s,
		corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})
	// The kubelet has not reported the helper at all yet: only the target's
	// status is present. An absent entry must count as still running.
	pod.Status.ContainerStatuses = pod.Status.ContainerStatuses[:1]

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, captureResolutionBackstop, result.RequeueAfter)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated),
		"a helper with no status entry must not be treated as finished")
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
}

// sidecarSnapshotJob builds a SnapshotJob whose pod template carries a native
// sidecar (init container with restartPolicy Always) as the only non-target
// container, plus the matching source pod with the target already killed by
// the checkpoint and the sidecar in the given state (nil = no status entry).
func sidecarSnapshotJob(t *testing.T, s *runtime.Scheme, sidecarState *corev1.ContainerState) (*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod, *snapshotv1alpha1.PodSnapshot) {
	t.Helper()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	always := corev1.ContainerRestartPolicyAlways
	sj.Spec.PodTemplate.Spec.InitContainers = append(sj.Spec.PodTemplate.Spec.InitContainers,
		corev1.Container{Name: "sidecar", Image: "test:latest", RestartPolicy: &always})
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "worker", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}},
	}
	if sidecarState != nil {
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "sidecar", State: *sidecarState}}
	}
	snap := readySnapshot(t, sj, pod)
	setJobFailureCondition(job, batchv1.JobFailed, "BackoffLimitExceeded", "checkpoint terminated the target container")
	return sj, job, pod, snap
}

func TestSnapshotJobReconcileSidecarStillRunningWaits(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	// A sidecar-only pod also exercises the removed single-container shortcut:
	// with no regular helpers, the sidecar must still gate completion.
	sj, job, pod, snap := sidecarSnapshotJob(t, s,
		&corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, captureResolutionBackstop, result.RequeueAfter)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated),
		"a still-running native sidecar must block completion so cleanup cannot kill it mid-work")
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
}

func TestSnapshotJobReconcileSidecarMissingStatusWaits(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj, job, pod, snap := sidecarSnapshotJob(t, s, nil)

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, captureResolutionBackstop, result.RequeueAfter)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
}

func TestSnapshotJobReconcileSidecarTerminatedNonZeroCompletes(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	// The kubelet SIGTERMs sidecars after the regular containers finish, so a
	// non-zero exit is normal shutdown, not a failed helper deliverable.
	sj, job, pod, snap := sidecarSnapshotJob(t, s,
		&corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 143}})

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.True(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
}

func TestSnapshotJobReconcileSidecarRestartAfterCacheReadIsNotTrusted(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	// The cache observed the sidecar mid-restart-cycle Terminated; the kubelet
	// has since brought it back up (native sidecars can cycle
	// Terminated -> Running, unlike regular containers). A terminal-looking
	// cache read is irreversible here (it drives Job deletion), so it must be
	// re-confirmed against a live read rather than trusted outright.
	sj, job, pod, snap := sidecarSnapshotJob(t, s,
		&corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})

	livePod := pod.DeepCopy()
	livePod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithObjects(livePod).Build()

	result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, captureResolutionBackstop, result.RequeueAfter)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated),
		"a stale cached Terminated must not complete the SnapshotJob when the sidecar is actually still running")
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
}

func TestSnapshotJobReconcileHelperUnverifiableFails(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj, job, _, snap := helperSnapshotJob(t, s,
		corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})
	// Pin the adopted PodSnapshot so the missing pod exercises helper
	// verification, not the pre-adoption identity check.
	snap.UID = types.UID("pod-snapshot-uid")
	sj.Status.PodSnapshotName = snap.Name
	sj.Status.PodSnapshotUID = snap.UID

	// The source pod is gone: helper completion cannot be verified.
	r := makeSnapshotJobReconciler(s, sj, job, snap)
	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonJobFailed, failed.Reason)
	assert.Contains(t, failed.Message, "verified")
}

func TestSnapshotJobReconcileReadyCaptureWithDeadlineFails(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	snap := readySnapshot(t, sj, pod)
	setJobFailureCondition(job, batchv1.JobFailureTarget, batchv1.JobReasonDeadlineExceeded, "Job was active longer than specified deadline")

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonDeadlineExceeded, failed.Reason,
		"the deadline bounds the whole source lifecycle, helpers included, even after a successful capture")
}

func TestSnapshotJobReconcileCapturedKeepsCaptureReasonUnderDeadline(t *testing.T) {
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
		Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue,
		Reason: "CRIUDumpFailed", Message: "dump failed",
	})
	setJobFailureCondition(job, batchv1.JobFailureTarget, batchv1.JobReasonDeadlineExceeded, "Job was active longer than specified deadline")

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	// One owner per condition: the deadline is the aggregate root cause, but it
	// must not rewrite the capture's own result.
	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonDeadlineExceeded, failed.Reason)
	captured := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, captured)
	assert.Equal(t, metav1.ConditionFalse, captured.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonCaptureFailed, captured.Reason,
		"Captured is owned by PodSnapshot.status; the Job's deadline must not overwrite it")
	assert.Contains(t, captured.Message, "dump failed")
}

func TestSnapshotJobReconcileHelperVerificationCacheMissDoesNotFail(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj, job, pod, snap := helperSnapshotJob(t, s,
		corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}})

	// The pod exists on the API server but not in the informer yet. Helper
	// verification must confirm the cached miss authoritatively instead of
	// writing the sticky unverifiable failure.
	r := makeSnapshotJobReconciler(s, sj, job, snap)
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithObjects(pod).Build()

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated),
		"a pod hidden by informer lag must not fail helper verification")
	assert.True(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
}

func TestDeriveCapturedStatusDoesNotDowngradeCapturedTrue(t *testing.T) {
	sj := minimalSnapshotJob()
	now := metav1.Now()
	setCondition(sj, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionTrue, now,
		snapshotv1alpha1.ReasonCaptureCompleted, "CRIU dump of the target container is complete")

	// The capture succeeded earlier; losing the PodSnapshot afterwards fails the
	// SnapshotJob but must not rewrite the recorded capture success.
	observed := terminalObservation(snapshotv1alpha1.ReasonPodSnapshotDeleted,
		errors.New("PodSnapshot no longer exists"))
	status := deriveSnapshotJobStatus(sj, observed, now)

	captured := meta.FindStatusCondition(status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, captured)
	assert.Equal(t, metav1.ConditionTrue, captured.Status)
	failed := meta.FindStatusCondition(status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonPodSnapshotDeleted, failed.Reason)
}

func TestSnapshotJobReconcileWaitsForCaptureWhenJobFailsDuringPendingCapture(t *testing.T) {
	for _, conditionType := range []batchv1.JobConditionType{batchv1.JobFailureTarget, batchv1.JobFailed} {
		t.Run(string(conditionType), func(t *testing.T) {
			s := snapshotJobReconcilerScheme()
			sj := minimalSnapshotJob()
			sj.UID = types.UID("sj-uid")
			job, err := buildSourceJob(sj)
			require.NoError(t, err)
			require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
			pod := sourcePodForJob(job)
			snap, err := buildPodSnapshot(sj, pod)
			require.NoError(t, err)
			setJobFailureCondition(job, conditionType, "BackoffLimitExceeded", "checkpoint terminated the source process")

			r := makeSnapshotJobReconciler(s, sj, job, pod, snap)
			result, err := r.Reconcile(context.Background(), reconcileRequest(sj))
			require.NoError(t, err)
			assert.Equal(t, captureResolutionBackstop, result.RequeueAfter,
				"a Job failure racing a pending capture must wait for the capture result, not fail")

			updated := &snapshotv1alpha1.SnapshotJob{}
			require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
			assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
			assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
			captured := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
			require.NotNil(t, captured)
			assert.Equal(t, metav1.ConditionFalse, captured.Status)
			assert.Equal(t, snapshotv1alpha1.ReasonCaptureInProgress, captured.Reason)

			// The agent (or the PodSnapshot reconciler's grace expiry) resolves
			// the capture; its failure detail is what the SnapshotJob reports.
			stored := &snapshotv1alpha1.PodSnapshot{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), stored))
			meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
				Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue,
				Reason: "CRIUDumpFailed", Message: "dump failed",
			})
			require.NoError(t, r.Update(context.Background(), stored))

			_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
			require.NoError(t, err)
			require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
			assert.True(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
			failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, snapshotv1alpha1.ReasonCaptureFailed, failed.Reason,
				"the capture's own failure detail must win over the raced Job failure")

			jobs := &batchv1.JobList{}
			require.NoError(t, r.List(context.Background(), jobs))
			assert.NotNil(t, getBatchJobByName(jobs, sj.Name), "failed source Job must be preserved for debugging")
		})
	}
}

func TestSnapshotJobReconcileDeadlineExceeded(t *testing.T) {
	for _, conditionType := range []batchv1.JobConditionType{batchv1.JobFailureTarget, batchv1.JobFailed} {
		t.Run(string(conditionType), func(t *testing.T) {
			s := snapshotJobReconcilerScheme()
			sj := minimalSnapshotJob()
			sj.UID = types.UID("sj-uid")
			job, err := buildSourceJob(sj)
			require.NoError(t, err)
			require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
			setJobFailureCondition(job, conditionType, batchv1.JobReasonDeadlineExceeded, "Job was active longer than specified deadline")

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
		})
	}
}

// ---- the remaining non-execution failure reasons ----

func TestSnapshotJobReconcileJobDeleted(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.Status.SourceJobUID = types.UID("deleted-job-uid")
	startedAt := metav1.Unix(1_700_000_000, 0)
	sj.Status.StartedAt = &startedAt
	meta.SetStatusCondition(&sj.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.SnapshotJobConditionRunning, Status: metav1.ConditionTrue,
		Reason: snapshotv1alpha1.ReasonPodReady, Message: "source pod is ready",
	})
	require.Empty(t, sj.Status.PodSnapshotName, "the UID must detect deletion before PodSnapshot status is recorded")

	r := makeSnapshotJobReconciler(s, sj)                                // cached Job lookup misses
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).Build() // the Job is authoritatively gone too
	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, snapshotv1alpha1.ReasonJobDeleted, cond.Reason)
	running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running)
	assert.Equal(t, metav1.ConditionFalse, running.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonJobDeleted, running.Reason)
	require.NotNil(t, updated.Status.StartedAt)
	assert.Equal(t, startedAt, *updated.Status.StartedAt)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.Empty(t, jobs.Items, "nothing to preserve — it was already gone")
}

func TestSnapshotJobReconcileFailedPodSnapshotAndDeletedJobUsesJobDeleted(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
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
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue,
		Reason: "SourcePodGone", Message: "source pod disappeared",
	})

	// The cached Job is stale, but once a capture result exists it is
	// authoritative: the capture failure detail is reported and the Job's
	// concurrent deletion never needs a confirming read.
	r := makeSnapshotJobReconciler(s, sj, job, snap)
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).Build()

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonCaptureFailed, failed.Reason,
		"the capture's own failure detail must be preserved over the source Job's fate")
}

// ---- a deleted source Job is terminal even with a Ready capture ----

func TestSnapshotJobReconcileReadyCaptureStaysJobDeletedWhenJobGone(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	sj.Status.SourceJobUID = types.UID("deleted-job-uid")
	sj.Status.PodSnapshotName = sj.Name
	sj.Status.PodSnapshotUID = types.UID("pod-snapshot-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	pod := sourcePodForJob(job)
	snap := readySnapshot(t, sj, pod)
	snap.UID = sj.Status.PodSnapshotUID

	// The Job is gone from both the cache and the API server. Even with the
	// capture durably Ready, completion cannot be declared: the two-stage gate
	// requires the source Job to finish, and a deleted Job can never confirm
	// that its helper containers completed their work.
	r := makeSnapshotJobReconciler(s, sj, snap)
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithObjects(snap).Build()

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobCompleted(updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonJobDeleted, failed.Reason)
}

func TestSnapshotJobReconcilePendingCaptureStaysJobDeletedWhenJobGone(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	sj.Status.SourceJobUID = types.UID("deleted-job-uid")
	sj.Status.PodSnapshotName = sj.Name
	sj.Status.PodSnapshotUID = types.UID("pod-snapshot-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod) // pending: no capture result
	require.NoError(t, err)
	snap.UID = sj.Status.PodSnapshotUID

	r := makeSnapshotJobReconciler(s, sj, snap)
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithObjects(snap).Build()

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonJobDeleted, failed.Reason,
		"one-shot: a deleted Job with an unresolved capture can never complete and must stay terminal")
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
		{"source completion without result keeps its specific reason", snapshotv1alpha1.ReasonSourceCompletedWithoutCapture, snapshotv1alpha1.ReasonSourceCompletedWithoutCapture},
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
	job.UID = types.UID("source-job-uid")
	sj.Status.SourceJobUID = job.UID

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

func TestSnapshotJobReconcileCompletionDoesNotDeleteReplacementJob(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.Status.SourceJobUID = types.UID("original-job-uid")
	meta.SetStatusCondition(&sj.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.SnapshotJobConditionCompleted, Status: metav1.ConditionTrue,
		Reason: snapshotv1alpha1.ReasonJobCompleted,
	})

	replacement, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, replacement, s))
	replacement.UID = types.UID("replacement-job-uid")
	r := makeSnapshotJobReconciler(s, sj, replacement)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	remaining := &batchv1.Job{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(replacement), remaining))
	assert.Equal(t, replacement.UID, remaining.UID, "cleanup must not delete a different Job incarnation")
}

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
