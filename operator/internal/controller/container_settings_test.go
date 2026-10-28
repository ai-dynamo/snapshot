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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func containerSettings(name string) []snapshotv1alpha1.ContainerSnapshotConfig {
	return []snapshotv1alpha1.ContainerSnapshotConfig{{
		Name: name,
		CheckpointReadinessProbe: &snapshotv1alpha1.CheckpointReadinessProbe{
			CheckpointReadinessProbeHandler: snapshotv1alpha1.CheckpointReadinessProbeHandler{
				File: &snapshotv1alpha1.ControlFileAction{Path: "/tmp/ready"},
			},
			PeriodSeconds: 2,
		},
		Lifecycle: &snapshotv1alpha1.SnapshotLifecycle{PostRestore: &snapshotv1alpha1.PostRestoreAction{
			PostRestoreHandler: snapshotv1alpha1.PostRestoreHandler{
				HTTPPost: &snapshotv1alpha1.LifecycleHTTPAction{Path: "/resume", Port: 8000},
			},
		}},
	}}
}

func TestUsesLifecycleHooks(t *testing.T) {
	assert.False(t, usesLifecycleHooks(nil))
	assert.False(t, usesLifecycleHooks([]snapshotv1alpha1.ContainerSnapshotConfig{{Name: "main"}}),
		"an entry with only a name changes nothing")
	probeOnly := containerSettings("main")
	probeOnly[0].Lifecycle = nil
	assert.True(t, usesLifecycleHooks(probeOnly))
	lifecycleOnly := containerSettings("main")
	lifecycleOnly[0].CheckpointReadinessProbe = nil
	assert.True(t, usesLifecycleHooks(lifecycleOnly))
}

func TestBuildPodSnapshotCarriesContainerSettings(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-worker-abcde", Namespace: "inference", UID: types.UID("pod-uid")},
	}
	sj := minimalSnapshotJob()
	sj.Spec.PodSnapshotTemplate.Containers = containerSettings("worker")

	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	assert.Equal(t, sj.Spec.PodSnapshotTemplate.Containers, snap.Spec.Containers)
	snap.Spec.Containers[0].CheckpointReadinessProbe.File.Path = "/tmp/changed"
	assert.Equal(t, "/tmp/ready", sj.Spec.PodSnapshotTemplate.Containers[0].CheckpointReadinessProbe.File.Path,
		"the PodSnapshot gets its own copy")

	sj.Spec.PodSnapshotTemplate.Containers = nil
	snap, err = buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	assert.Nil(t, snap.Spec.Containers)
}

func TestSnapshotReconciler_BuildPodSnapshotContentSavesContainerSettings(t *testing.T) {
	r := makeSnapshotReconciler(snapshotReconcilerScheme())
	snap := makeSnapshotForReconcile()
	snap.Spec.Source.PodRef.Containers = []string{"main"}
	snap.Spec.Containers = containerSettings("main")

	content := r.buildPodSnapshotContent(snap, "content-x", scheduledPod())
	assert.Equal(t, snap.Spec.Containers, content.Spec.Containers)
	content.Spec.Containers[0].Lifecycle.PostRestore.HTTPPost.Port = 9000
	assert.Equal(t, int32(8000), snap.Spec.Containers[0].Lifecycle.PostRestore.HTTPPost.Port,
		"the content gets its own copy")

	snap.Spec.Containers = nil
	assert.Nil(t, r.buildPodSnapshotContent(snap, "content-x", scheduledPod()).Spec.Containers)
}

func TestSnapshotReconciler_LifecycleHooksGate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		settings    []snapshotv1alpha1.ContainerSnapshotConfig
		enabled     bool
		wantContent bool
	}{
		{"disabled rejects settings", containerSettings("main"), false, false},
		{"disabled allows a name-only entry", []snapshotv1alpha1.ContainerSnapshotConfig{{Name: "main"}}, false, true},
		{"disabled allows no entry", nil, false, true},
		{"enabled accepts settings", containerSettings("main"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := makeSnapshotForReconcile()
			snap.Spec.Source.PodRef.Containers = []string{"main"}
			snap.Spec.Containers = tc.settings
			r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, scheduledPod())
			r.EnableLifecycleHooks = tc.enabled

			reconcileSnapshot(t, r, snap.Name)

			var contents snapshotv1alpha1.PodSnapshotContentList
			require.NoError(t, r.List(context.Background(), &contents))
			updated := &snapshotv1alpha1.PodSnapshot{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "inference", Name: snap.Name}, updated))
			failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
			if !tc.wantContent {
				assert.Empty(t, contents.Items, "a rejected request must not create content")
				require.NotNil(t, failed)
				assert.Equal(t, metav1.ConditionTrue, failed.Status)
				assert.Equal(t, podSnapshotReasonLifecycleHooksDisabled, failed.Reason)
				assert.Contains(t, failed.Message, "--enable-lifecycle-hooks")
				return
			}
			assert.Nil(t, failed)
			require.Len(t, contents.Items, 1)
			assert.Equal(t, tc.settings, contents.Items[0].Spec.Containers)
		})
	}
}

func TestSnapshotJobReconcileLifecycleHooksGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{"disabled rejects settings before the source Job", false},
		{"enabled creates the source Job", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sj := minimalSnapshotJob()
			sj.Spec.PodSnapshotTemplate.Containers = containerSettings("worker")
			r := makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj)
			r.EnableLifecycleHooks = tc.enabled

			_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
			require.NoError(t, err)

			jobs := &batchv1.JobList{}
			require.NoError(t, r.List(context.Background(), jobs))
			updated := &snapshotv1alpha1.SnapshotJob{}
			require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
			if tc.enabled {
				assert.Len(t, jobs.Items, 1)
				assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
				return
			}
			assert.Empty(t, jobs.Items, "the workload must not start for a request that would fail")
			failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, snapshotv1alpha1.ReasonInvalidSpec, failed.Reason)
			assert.Contains(t, failed.Message, "--enable-lifecycle-hooks")
		})
	}
}
