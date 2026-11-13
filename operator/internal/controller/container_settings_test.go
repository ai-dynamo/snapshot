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
	"k8s.io/apimachinery/pkg/util/intstr"

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
				HTTPPost: &snapshotv1alpha1.HTTPPostAction{Path: "/resume", Port: intstr.FromInt32(8000)},
			},
		}},
	}}
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
	content.Spec.Containers[0].Lifecycle.PostRestore.HTTPPost.Port = intstr.FromInt32(9000)
	assert.Equal(t, intstr.FromInt32(8000), snap.Spec.Containers[0].Lifecycle.PostRestore.HTTPPost.Port,
		"the content gets its own copy")

	snap.Spec.Containers = nil
	assert.Nil(t, r.buildPodSnapshotContent(snap, "content-x", scheduledPod()).Spec.Containers)
}

func TestSnapshotReconciler_SavesContainerSettingsInTheContent(t *testing.T) {
	snap := makeSnapshotForReconcile()
	snap.Spec.Source.PodRef.Containers = []string{"main"}
	snap.Spec.Containers = containerSettings("main")
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, scheduledPod())

	reconcileSnapshot(t, r, snap.Name)

	var contents snapshotv1alpha1.PodSnapshotContentList
	require.NoError(t, r.List(context.Background(), &contents))
	require.Len(t, contents.Items, 1)
	assert.Equal(t, snap.Spec.Containers, contents.Items[0].Spec.Containers)
	updated := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "inference", Name: snap.Name}, updated))
	assert.Nil(t, meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed))
}

func TestSnapshotJobReconcileAcceptsContainerSettings(t *testing.T) {
	sj := minimalSnapshotJob()
	sj.Spec.PodSnapshotTemplate.Containers = containerSettings("worker")
	r := makeSnapshotJobReconciler(snapshotJobReconcilerScheme(), sj)

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.Len(t, jobs.Items, 1)
	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
}
