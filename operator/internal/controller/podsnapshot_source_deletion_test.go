// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func startedBoundSnapshotContent() (*snapshotv1alpha1.PodSnapshot, *snapshotv1alpha1.PodSnapshotContent) {
	snap, content := pendingBoundSnapshotContent()
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.PodSnapshotConditionStarted, Status: metav1.ConditionTrue,
		Reason: "CheckpointStarted", LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
	})
	return snap, content
}

func TestSnapshotReconciler_SourceLostBeforeStartedFailsImmediately(t *testing.T) {
	for _, state := range []string{"deleted", "replaced", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			snap, content := pendingBoundSnapshotContent()
			objects := []client.Object{snap, content}
			pod := scheduledPod()
			reason := "SourcePodNotFound"
			switch state {
			case "replaced":
				pod.UID = "replacement-uid"
				reason = "StalePodReference"
			case "succeeded":
				pod.Status.Phase = corev1.PodSucceeded
				reason = snapshotv1alpha1.ReasonSourceCompletedWithoutCapture
			}
			if state != "deleted" {
				objects = append(objects, pod)
			}
			r := makeSnapshotReconciler(snapshotReconcilerScheme(), objects...)
			assert.Zero(t, reconcileSnapshot(t, r, snap.Name).RequeueAfter)
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), snap))
			failed := meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, reason, failed.Reason)
		})
	}
}

// Stop between source exit and publication, reconcile the deletion first, then
// publish the result. No timing or informer arrival order can hide the race.
func TestSnapshotReconciler_SourceDeletionBeforeResult(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "deleted"
		if replacement {
			name = "recreated under the same name"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			snap, content := startedBoundSnapshotContent()
			snap.Status.Conditions = []metav1.Condition{{
				Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionFalse,
				Reason: "Pending", LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
			}}
			pod := scheduledPod()
			r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, content, pod)
			reconcileSnapshot(t, r, snap.Name)
			require.NoError(t, r.Delete(ctx, pod))
			if replacement {
				pod = scheduledPod()
				pod.UID = "replacement-uid"
				require.NoError(t, r.Create(ctx, pod))
			}

			res := reconcileSnapshot(t, r, snap.Name)
			require.Positive(t, res.RequeueAfter)
			pending := &snapshotv1alpha1.PodSnapshot{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), pending))
			require.False(t, snapshotv1alpha1.IsPodSnapshotFailed(pending))
			condition := meta.FindStatusCondition(pending.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
			require.NotNil(t, condition)
			require.Equal(t, metav1.ConditionUnknown, condition.Status)
			started := condition.LastTransitionTime

			// A fresh reconciler has no remembered clock. The recorded condition
			// must keep the original expiry, even across another reconcile.
			restarted := &PodSnapshotReconciler{Client: r.Client, NonCacheReadClient: r.NonCacheReadClient, Recorder: r.Recorder}
			reconcileSnapshot(t, restarted, snap.Name)
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), pending))
			assert.Equal(t, started, meta.FindStatusCondition(pending.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady).LastTransitionTime)

			meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
				Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured", Message: "dump and artifact commit succeeded",
			})
			require.NoError(t, r.Status().Update(ctx, content))
			reconcileSnapshot(t, restarted, snap.Name)
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), pending))
			assert.True(t, snapshotv1alpha1.IsPodSnapshotSucceeded(pending))
			assert.False(t, snapshotv1alpha1.IsPodSnapshotFailed(pending))
			assert.Equal(t, snap.Spec.Source, pending.Spec.Source)
		})
	}
}

func TestSnapshotReconciler_DeletedSourceWaitExpiresAfterRestart(t *testing.T) {
	ctx := context.Background()
	snap, content := startedBoundSnapshotContent()
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, content)
	require.Positive(t, reconcileSnapshot(t, r, snap.Name).RequeueAfter)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), snap))
	ready := meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, ready)
	ready.LastTransitionTime = metav1.NewTime(time.Now().Add(-sourcePodTerminalGrace - time.Minute))
	require.NoError(t, r.Status().Update(ctx, snap))

	restarted := &PodSnapshotReconciler{Client: r.Client, NonCacheReadClient: r.NonCacheReadClient, Recorder: r.Recorder}
	assert.Zero(t, reconcileSnapshot(t, restarted, snap.Name).RequeueAfter)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), snap))
	require.True(t, snapshotv1alpha1.IsPodSnapshotFailed(snap))
	assert.Equal(t, "SourcePodNotFound", meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
}

func TestSnapshotReconciler_SourceDeletionKeepsTerminalClock(t *testing.T) {
	ctx := context.Background()
	snap, content := startedBoundSnapshotContent()
	pod := scheduledPod()
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, FinishedAt: metav1.NewTime(time.Now().Add(-time.Minute))},
	}}}
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, content, pod)
	require.Positive(t, reconcileSnapshot(t, r, snap.Name).RequeueAfter)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), snap))
	condition := meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, condition)
	started := condition.LastTransitionTime
	require.NoError(t, r.Delete(ctx, pod))
	require.Positive(t, reconcileSnapshot(t, r, snap.Name).RequeueAfter)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), snap))
	assert.Equal(t, started, meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady).LastTransitionTime)
}

func TestSnapshotReconciler_DeletedSourceWaitSurvivesStaleLiveCache(t *testing.T) {
	ctx := context.Background()
	snap, content := startedBoundSnapshotContent()
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, content)
	require.Positive(t, reconcileSnapshot(t, r, snap.Name).RequeueAfter)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), snap))
	started := meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady).LastTransitionTime

	// An old live source can still be cached after its deletion is confirmed.
	// The API reader sees that it is gone and must keep the original clock.
	require.NoError(t, r.Create(ctx, scheduledPod()))
	r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(snapshotReconcilerScheme()).WithObjects(content).Build()
	require.Positive(t, reconcileSnapshot(t, r, snap.Name).RequeueAfter)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(snap), snap))
	ready := meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionUnknown, ready.Status)
	assert.Equal(t, started, ready.LastTransitionTime)
}
