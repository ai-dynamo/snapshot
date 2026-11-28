// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	v1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func verificationFixture() (*v1.PodSnapshot, *v1.PodSnapshotContent, *corev1.Pod) {
	s, c := pendingBoundSnapshotContent()
	c.UID = "content-uid"
	s.Spec.Source.PodRef.Containers = []string{"main"}
	c.Spec.Source.PodRef.Containers = []string{"main"}
	s.Spec.RestoreVerification = &v1.RestoreVerificationPolicy{
		PodRef:                   v1.RestoreVerificationPodReference{Name: "canary", UID: "canary-uid"},
		ObservationWindowSeconds: 30, TimeoutSeconds: 120,
	}
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
		Type: v1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured",
	})
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: s.Namespace, UID: "canary-uid",
			Annotations: map[string]string{podcontract.RestoreFromAnnotation: s.Name}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main",
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8000)},
			}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "main", ContainerID: "containerd://canary-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}},
	}
	return s, c, p
}

func readVerificationSnapshot(t *testing.T, r *PodSnapshotReconciler, s *v1.PodSnapshot) *v1.PodSnapshot {
	t.Helper()
	got := &v1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(s), got))
	return got
}

func readyVerificationPod(p *corev1.Pod) {
	p.Status.ContainerStatuses[0].Ready = true
	p.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodConditionType(podcontract.RestoredCondition), Status: corev1.ConditionTrue, Reason: podcontract.RestoreReasonSucceeded},
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
}

func updateVerificationPod(t *testing.T, r *PodSnapshotReconciler, p *corev1.Pod) {
	t.Helper()
	status := p.Status.DeepCopy()
	require.NoError(t, r.Update(context.Background(), p))
	p.Status = *status
	require.NoError(t, r.Status().Update(context.Background(), p))
}

func TestRestoreVerificationRequiresObservedRestoreAndWindow(t *testing.T) {
	s, c, p := verificationFixture()
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), s, c, p)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	for range 3 {
		reconcileSnapshot(t, r, s.Name)
	}
	got := readVerificationSnapshot(t, r, s)
	require.Equal(t, v1.PodSnapshotPhaseRestoreVerifying, got.Status.Phase)
	require.True(t, v1.IsPodSnapshotSucceeded(got), "Ready retains checkpoint semantics")
	require.Equal(t, "containerd://canary-1", got.Status.RestoreVerifyInfo.Containers[0].ContainerID)
	require.Nil(t, got.Status.RestoreVerifyInfo.HealthySince, "Running without restore cannot verify")
	deadline := got.Status.RestoreVerifyInfo.DeadlineAt

	readyVerificationPod(p)
	updateVerificationPod(t, r, p)
	reconcileSnapshot(t, r, s.Name)
	got = readVerificationSnapshot(t, r, s)
	require.Equal(t, v1.PodSnapshotPhaseRestoreVerifying, got.Status.Phase)
	require.NotNil(t, got.Status.RestoreVerifyInfo.HealthySince)
	require.True(t, now.Equal(got.Status.RestoreVerifyInfo.HealthySince.Time))
	now = now.Add(29 * time.Second)
	// A new reconciler has no in-memory timer but must retain the original window.
	r2 := &PodSnapshotReconciler{Client: r.Client, NonCacheReadClient: r.Client, Recorder: r.Recorder, now: func() time.Time { return now }}
	reconcileSnapshot(t, r2, s.Name)
	require.Equal(t, v1.PodSnapshotPhaseRestoreVerifying, readVerificationSnapshot(t, r2, s).Status.Phase)
	now = now.Add(time.Second)
	reconcileSnapshot(t, r2, s.Name)
	got = readVerificationSnapshot(t, r2, s)
	require.Equal(t, v1.PodSnapshotPhaseCompleted, got.Status.Phase)
	require.True(t, v1.IsRestoreVerified(got, c.UID))
	require.Equal(t, deadline, got.Status.RestoreVerifyInfo.DeadlineAt)
	require.NoError(t, r2.Delete(context.Background(), p))
	reconcileSnapshot(t, r2, s.Name)
	require.Equal(t, got.Status.RestoreVerifyInfo, readVerificationSnapshot(t, r2, s).Status.RestoreVerifyInfo)
}

func TestAuxiliaryFailurePreservesCheckpointConditions(t *testing.T) {
	s, c, p := verificationFixture()
	s.Status.Phase = v1.PodSnapshotPhaseCheckpointSucceeded
	s.Labels = map[string]string{v1.SnapshotJobOwnerLabel: "producer", v1.SnapshotJobOwnerUIDLabel: "producer-uid"}
	j := &v1.SnapshotJob{ObjectMeta: metav1.ObjectMeta{Name: "producer", Namespace: s.Namespace, UID: "producer-uid"}}
	j.Status.PodSnapshotName, j.Status.PodSnapshotUID = s.Name, s.UID
	meta.SetStatusCondition(&j.Status.Conditions, metav1.Condition{Type: v1.SnapshotJobConditionFailed, Status: metav1.ConditionTrue, Reason: "JobFailed"})
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), s, c, p, j)
	for range 2 {
		reconcileSnapshot(t, r, s.Name)
		got := readVerificationSnapshot(t, r, s)
		require.Equal(t, v1.PodSnapshotPhaseFailed, got.Status.Phase)
		require.True(t, v1.IsPodSnapshotSucceeded(got), "checkpoint remains successful when auxiliary work fails")
		require.False(t, v1.IsPodSnapshotFailed(got))
		require.Nil(t, got.Status.RestoreVerifyInfo, "verification must not start")
	}
}

func TestRestoreVerificationRejectsStaleOrFailedCanary(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		mutate       func(*corev1.Pod)
	}{
		{"pod recreation", "VerificationPodChanged", func(p *corev1.Pod) { p.UID = "replacement" }},
		{"container replacement", "VerificationContainerChanged", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "containerd://other" }},
		{"container restart", "VerificationContainerChanged", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount++ }},
		{"restore failure", "VerificationRestoreFailed", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodConditionType(podcontract.RestoredCondition), Status: corev1.ConditionFalse, Reason: podcontract.RestoreReasonFailed}}
		}},
		{"crash", "VerificationContainerExited", func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, p := verificationFixture()
			r := makeSnapshotReconciler(snapshotReconcilerScheme(), s, c, p)
			for range 3 {
				reconcileSnapshot(t, r, s.Name)
			}
			tc.mutate(p)
			updateVerificationPod(t, r, p)
			reconcileSnapshot(t, r, s.Name)
			got := readVerificationSnapshot(t, r, s)
			require.Equal(t, v1.PodSnapshotPhaseVerificationFailed, got.Status.Phase)
			require.Equal(t, tc.reason, got.Status.RestoreVerifyInfo.Reason)
			require.False(t, v1.IsRestoreVerified(got, c.UID))
			require.True(t, v1.IsPodSnapshotSucceeded(got), "verification failure must not rewrite checkpoint result")
		})
	}
}

func TestRestoreVerificationTimeoutAndZeroWindow(t *testing.T) {
	s, c, p := verificationFixture()
	s.Spec.RestoreVerification.ObservationWindowSeconds = 0
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), s, c, p)
	now := time.Now().UTC().Truncate(time.Second)
	r.now = func() time.Time { return now }
	for range 3 {
		reconcileSnapshot(t, r, s.Name)
	}
	require.Equal(t, v1.PodSnapshotPhaseRestoreVerifying, readVerificationSnapshot(t, r, s).Status.Phase)
	now = now.Add(120 * time.Second)
	reconcileSnapshot(t, r, s.Name)
	got := readVerificationSnapshot(t, r, s)
	require.Equal(t, v1.PodSnapshotPhaseVerificationFailed, got.Status.Phase)
	require.Equal(t, "VerificationTimeout", got.Status.RestoreVerifyInfo.Reason)
}

func TestRestoreVerificationRequiresApplicationProbe(t *testing.T) {
	s, c, p := verificationFixture()
	p.Spec.Containers[0].ReadinessProbe = nil
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), s, c, p)
	for range 3 {
		reconcileSnapshot(t, r, s.Name)
	}
	got := readVerificationSnapshot(t, r, s)
	require.Equal(t, v1.PodSnapshotPhaseVerificationFailed, got.Status.Phase)
	require.Equal(t, "VerificationProbeMissing", got.Status.RestoreVerifyInfo.Reason)
}

func TestSnapshotLifecyclePersistsCheckpointProgress(t *testing.T) {
	s, c := pendingBoundSnapshotContent()
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), s, c, scheduledPod())
	reconcileSnapshot(t, r, s.Name)
	require.Equal(t, v1.PodSnapshotPhasePending, readVerificationSnapshot(t, r, s).Status.Phase)
	started := metav1.Now()
	c.Status.CheckpointStartedAt = &started
	require.NoError(t, r.Status().Update(context.Background(), c))
	reconcileSnapshot(t, r, s.Name)
	require.Equal(t, v1.PodSnapshotPhaseCheckpointing, readVerificationSnapshot(t, r, s).Status.Phase)
	meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{Type: v1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured"})
	require.NoError(t, r.Status().Update(context.Background(), c))
	reconcileSnapshot(t, r, s.Name)
	require.Equal(t, v1.PodSnapshotPhaseCheckpointSucceeded, readVerificationSnapshot(t, r, s).Status.Phase)
	reconcileSnapshot(t, r, s.Name)
	got := readVerificationSnapshot(t, r, s)
	require.Equal(t, v1.PodSnapshotPhaseCompleted, got.Status.Phase)
	require.Nil(t, got.Status.RestoreVerifyInfo, "disabled verification must not fabricate success")
}

func TestRestoreVerificationWaitsForSnapshotJobAuxiliaryCompletion(t *testing.T) {
	s, c, p := verificationFixture()
	s.Labels = map[string]string{v1.SnapshotJobOwnerLabel: "job", v1.SnapshotJobOwnerUIDLabel: "job-uid"}
	j := &v1.SnapshotJob{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: s.Namespace, UID: "job-uid"}, Status: v1.SnapshotJobStatus{PodSnapshotName: s.Name, PodSnapshotUID: s.UID}}
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), s, c, p, j)
	for range 3 {
		reconcileSnapshot(t, r, s.Name)
	}
	got := readVerificationSnapshot(t, r, s)
	require.Equal(t, v1.PodSnapshotPhaseCheckpointSucceeded, got.Status.Phase)
	require.Nil(t, got.Status.RestoreVerifyInfo)
	meta.SetStatusCondition(&j.Status.Conditions, metav1.Condition{Type: v1.SnapshotJobConditionCompleted, Status: metav1.ConditionTrue, Reason: v1.ReasonJobCompleted})
	require.NoError(t, r.Update(context.Background(), j))
	for range 3 {
		reconcileSnapshot(t, r, s.Name)
	}
	got = readVerificationSnapshot(t, r, s)
	require.Equal(t, v1.PodSnapshotPhaseRestoreVerifying, got.Status.Phase)
	require.Len(t, got.Status.RestoreVerifyInfo.Containers, 1)
}

func TestRestoreVerificationZeroWindowStillRequiresRestoreAndReadiness(t *testing.T) {
	s, c, p := verificationFixture()
	s.Spec.RestoreVerification.ObservationWindowSeconds = 0
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), s, c, p)
	for range 4 {
		reconcileSnapshot(t, r, s.Name)
	}
	require.Equal(t, v1.PodSnapshotPhaseRestoreVerifying, readVerificationSnapshot(t, r, s).Status.Phase)
	readyVerificationPod(p)
	p.Status.ContainerStatuses[0].Ready = false
	updateVerificationPod(t, r, p)
	reconcileSnapshot(t, r, s.Name)
	require.Equal(t, v1.PodSnapshotPhaseRestoreVerifying, readVerificationSnapshot(t, r, s).Status.Phase)
	p.Status.ContainerStatuses[0].Ready = true
	updateVerificationPod(t, r, p)
	reconcileSnapshot(t, r, s.Name)
	require.True(t, v1.IsRestoreVerified(readVerificationSnapshot(t, r, s), c.UID))
}

func TestRestoreVerificationReadinessLossRestartsWindow(t *testing.T) {
	s, c, p := verificationFixture()
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), s, c, p)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	for range 4 {
		reconcileSnapshot(t, r, s.Name)
	}
	readyVerificationPod(p)
	updateVerificationPod(t, r, p)
	reconcileSnapshot(t, r, s.Name)
	now = now.Add(20 * time.Second)
	p.Status.ContainerStatuses[0].Ready = false
	updateVerificationPod(t, r, p)
	reconcileSnapshot(t, r, s.Name)
	require.Nil(t, readVerificationSnapshot(t, r, s).Status.RestoreVerifyInfo.HealthySince)
	p.Status.ContainerStatuses[0].Ready = true
	updateVerificationPod(t, r, p)
	reconcileSnapshot(t, r, s.Name)
	now = now.Add(10 * time.Second)
	reconcileSnapshot(t, r, s.Name)
	require.Equal(t, v1.PodSnapshotPhaseRestoreVerifying, readVerificationSnapshot(t, r, s).Status.Phase)
	now = now.Add(20 * time.Second)
	reconcileSnapshot(t, r, s.Name)
	require.True(t, v1.IsRestoreVerified(readVerificationSnapshot(t, r, s), c.UID))
}
