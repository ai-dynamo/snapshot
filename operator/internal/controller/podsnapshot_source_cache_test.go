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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// The Pod cache can be behind the API server. A source pod that it does not
// hold yet, or still holds as the pod it replaced, must not fail a PodSnapshot
// for good: Failed is sticky, and the pod is there.
func TestSnapshotReconciler_UnboundSourceBehindInCacheIsReadFromTheAPIServer(t *testing.T) {
	s := snapshotReconcilerScheme()
	for _, tc := range []struct {
		name       string
		pinnedUID  types.UID
		cached     *corev1.Pod
		live       *corev1.Pod
		wantReason string
		wantBound  bool
	}{
		{
			name:      "pod not in the cache yet",
			live:      scheduledPod(),
			wantBound: true,
		},
		{
			name: "pod not in the cache yet and not scheduled",
			live: func() *corev1.Pod {
				pod := scheduledPod()
				pod.Spec.NodeName = ""
				return pod
			}(),
			wantReason: podSnapshotReasonSourcePodUnscheduled,
		},
		{
			name:      "cache still holds the pod that the pinned one replaced",
			pinnedUID: "pod-uid-9",
			cached: func() *corev1.Pod {
				pod := scheduledPod()
				pod.UID = "pod-uid-8"
				return pod
			}(),
			live:      scheduledPod(),
			wantBound: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := makeSnapshotForReconcile()
			snap.Spec.Source.PodRef.UID = tc.pinnedUID
			cached := []client.Object{snap}
			if tc.cached != nil {
				cached = append(cached, tc.cached)
			}
			r := makeSnapshotReconciler(s, cached...)
			r.NonCacheReadClient = fake.NewClientBuilder().WithScheme(s).WithObjects(tc.live).Build()

			reconcileSnapshot(t, r, snap.Name)

			updated := &snapshotv1alpha1.PodSnapshot{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(snap), updated))
			failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
			require.Nil(t, failed, "failed for good although the source pod exists: %+v", failed)
			if tc.wantReason != "" {
				ready := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
				require.NotNil(t, ready)
				assert.Equal(t, tc.wantReason, ready.Reason)
			}
			if !tc.wantBound {
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
