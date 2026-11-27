// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func TestSourcePodCapturePredicateUpdate(t *testing.T) {
	deleting := metav1.Now()
	tests := []struct {
		name   string
		change func(*corev1.Pod)
		want   bool
	}{
		{name: "status churn is dropped", change: func(pod *corev1.Pod) {
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", Ready: true}}
		}},
		{name: "scheduling is admitted", change: func(pod *corev1.Pod) { pod.Spec.NodeName = "node-b" }, want: true},
		{name: "a phase change is admitted", change: func(pod *corev1.Pod) { pod.Status.Phase = corev1.PodFailed }, want: true},
		{name: "the start of deletion is admitted", change: func(pod *corev1.Pod) { pod.DeletionTimestamp = &deleting }, want: true},
		// The informer holds the old pod, the list after a watch gap holds the new one, and the
		// two reach the handler as one update. Nothing else differs when both are in one state.
		{name: "a same-name recreation in the same state is admitted", change: func(pod *corev1.Pod) { pod.UID = "pod-uid-10" }, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldPod := scheduledPod()
			oldPod.Status.Phase = corev1.PodRunning
			newPod := oldPod.DeepCopy()
			test.change(newPod)

			assert.Equal(t, test.want,
				sourcePodCapturePredicate().Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}))
		})
	}
}

// A PodSnapshot that waits for an unscheduled pod has the pod watch as its only wake-up. When a
// relist replaces that pod with a same-name recreation that is also unscheduled, the update must
// still reach the PodSnapshot, which then fails the stale work order.
func TestSnapshotReconciler_ReplacedUnscheduledSourcePodFailsPinnedSnapshot(t *testing.T) {
	s := snapshotReconcilerScheme()
	snap := makeSnapshotForReconcile()
	snap.Spec.Source.PodRef.UID = types.UID("pod-uid-9")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "inference", UID: types.UID("pod-uid-9")}}
	r := makeSnapshotReconciler(s, snap, pod)
	key := types.NamespacedName{Namespace: "inference", Name: snap.Name}

	reconcileSnapshot(t, r, snap.Name)
	waiting := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), key, waiting))
	ready := meta.FindStatusCondition(waiting.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, ready)
	require.Equal(t, podSnapshotReasonSourcePodUnscheduled, ready.Reason)

	replacement := pod.DeepCopy()
	replacement.UID = types.UID("pod-uid-10")
	replacement.ResourceVersion = ""
	require.NoError(t, r.Delete(context.Background(), pod))
	require.NoError(t, r.Create(context.Background(), replacement))

	require.True(t, sourcePodCapturePredicate().Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: replacement}),
		"the update that carries a same-name recreation must pass the predicate")
	requests := r.sourcePodToPodSnapshots(context.Background(), replacement)
	require.Equal(t, []reconcile.Request{{NamespacedName: key}}, requests)
	_, err := r.Reconcile(context.Background(), requests[0])
	require.NoError(t, err)

	failed := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), key, failed))
	cond := meta.FindStatusCondition(failed.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, "StalePodReference", cond.Reason)
}
