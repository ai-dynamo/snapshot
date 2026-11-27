//go:build linux

// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvidiacomv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// snapshotScheme returns a scheme with the operator CRD and core types registered.
func snapshotScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, nvidiacomv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func TestCaptureJobName(t *testing.T) {
	short := captureJobName("my-snapshot")
	assert.True(t, strings.HasPrefix(short, "my-snapshot-capture-"))
	assert.LessOrEqual(t, len(short), 63)

	long := captureJobName(strings.Repeat("x", 100))
	assert.Len(t, long, 63)
	assert.Contains(t, long, "-capture-")
}

// TestDeleteCheckpointJobDeletesSourcePod verifies that cleaning up a failed checkpoint asks the
// API server to delete the Job's Pods with it. A batch/v1 Job delete that names no propagation
// policy orphans them, which leaves the source Pod running.
func TestDeleteCheckpointJobDeletesSourcePod(t *testing.T) {
	clientset := k8sfake.NewClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snapshot-capture-abcde", Namespace: "default"},
	})

	require.NoError(t, deleteCheckpointJob(context.Background(), clientset, "default", "my-snapshot-capture-abcde"))

	var deletes []clienttesting.DeleteActionImpl
	for _, action := range clientset.Actions() {
		if deletion, ok := action.(clienttesting.DeleteActionImpl); ok {
			deletes = append(deletes, deletion)
		}
	}
	require.Len(t, deletes, 1, "the Job must be deleted once")
	assert.Equal(t, "my-snapshot-capture-abcde", deletes[0].GetName())
	policy := deletes[0].DeleteOptions.PropagationPolicy
	require.NotNil(t, policy, "a Job delete without a propagation policy orphans the source Pod")
	assert.Equal(t, metav1.DeletePropagationBackground, *policy)
}

// checkpointJobClientset returns a clientset that holds the Job of a checkpoint, and that answers
// the first Job deletes with the given errors, one per delete, before it deletes the Job.
func checkpointJobClientset(deleteErrs ...error) *k8sfake.Clientset {
	clientset := k8sfake.NewClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snapshot-capture-abcde", Namespace: "default"},
	})
	clientset.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		if len(deleteErrs) == 0 {
			return false, nil, nil
		}
		err := deleteErrs[0]
		deleteErrs = deleteErrs[1:]
		return true, nil, err
	})
	return clientset
}

func jobDeletes(clientset *k8sfake.Clientset) int {
	deletes := 0
	for _, action := range clientset.Actions() {
		if action.Matches("delete", "jobs") {
			deletes++
		}
	}
	return deletes
}

// TestCleanUpCheckpointJobOutlastsUnansweredDelete verifies that the cleanup of a failed
// checkpoint deletes the Job when the first deletes get no answer or a "try again" answer. The
// API server that made the checkpoint fail is the one that must accept the delete.
func TestCleanUpCheckpointJobOutlastsUnansweredDelete(t *testing.T) {
	clientset := checkpointJobClientset(
		errors.New(`Delete "https://kubernetes.example/apis/batch/v1/namespaces/default/jobs/my-snapshot-capture-abcde": connection refused`),
		apierrors.NewServiceUnavailable("apiserver starting"),
	)

	err := cleanUpCheckpointJob(context.Background(), clientset, "default", "my-snapshot-capture-abcde", time.Millisecond, 5*time.Second)

	require.NoError(t, err)
	assert.Equal(t, 3, jobDeletes(clientset), "two failed deletes, then the one that succeeds")
	_, getErr := clientset.BatchV1().Jobs("default").Get(context.Background(), "my-snapshot-capture-abcde", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(getErr), "the Job must be gone, got %v", getErr)
}

// TestCleanUpCheckpointJobNamesJobLeftBehind verifies that a cleanup that gives up says which Job
// it left and how to delete it. The Job's name ends in a random suffix that nothing else prints,
// and its source Pod holds a GPU until the Job is deleted.
func TestCleanUpCheckpointJobNamesJobLeftBehind(t *testing.T) {
	unavailable := apierrors.NewServiceUnavailable("apiserver starting")
	clientset := checkpointJobClientset(unavailable, unavailable, unavailable, unavailable, unavailable,
		unavailable, unavailable, unavailable, unavailable, unavailable, unavailable, unavailable)

	err := cleanUpCheckpointJob(context.Background(), clientset, "default", "my-snapshot-capture-abcde", 20*time.Millisecond, 100*time.Millisecond)

	require.Error(t, err)
	assert.Greater(t, jobDeletes(clientset), 1, "the delete must be retried until the cleanup timeout")
	assert.Contains(t, err.Error(), "default/my-snapshot-capture-abcde")
	assert.Contains(t, err.Error(), "kubectl delete job my-snapshot-capture-abcde --namespace default")
	assert.Contains(t, err.Error(), "apiserver starting", "the error must carry the API server's last answer")
}

// TestCleanUpCheckpointJobStopsOnRefusal verifies that a delete the API server refuses is not
// retried, because a later attempt gets the same answer, and that the error still names the Job.
func TestCleanUpCheckpointJobStopsOnRefusal(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "my-snapshot-capture-abcde", errors.New("no access"))
	clientset := checkpointJobClientset(forbidden, forbidden)

	err := cleanUpCheckpointJob(context.Background(), clientset, "default", "my-snapshot-capture-abcde", time.Millisecond, 5*time.Second)

	require.Error(t, err)
	assert.Equal(t, 1, jobDeletes(clientset), "a refused delete must not be retried")
	assert.True(t, apierrors.IsForbidden(err), "the refusal must stay in the error chain, got %v", err)
	assert.Contains(t, err.Error(), "kubectl delete job my-snapshot-capture-abcde --namespace default")
}

// TestCleanUpCheckpointJobAcceptsMissingJob verifies that a Job that is already gone counts as
// cleaned up. A delete whose answer was lost succeeded, and the retry then finds no Job.
func TestCleanUpCheckpointJobAcceptsMissingJob(t *testing.T) {
	clientset := k8sfake.NewClientset()

	err := cleanUpCheckpointJob(context.Background(), clientset, "default", "my-snapshot-capture-abcde", time.Millisecond, 5*time.Second)

	require.NoError(t, err)
	assert.Equal(t, 1, jobDeletes(clientset))
}

// TestPodSnapshotNameUIDPinned verifies that createPodSnapshot stamps the source pod's UID
// onto spec.source.podRef.uid so the operator rejects a same-named replacement pod.
func TestPodSnapshotNameUIDPinned(t *testing.T) {
	s := snapshotScheme(t)
	podUID := types.UID("pod-uid-abc")

	tests := []struct {
		name     string
		snapName string
	}{
		{name: "short name", snapName: "my-snap"},
		{name: "long PodSnapshot name", snapName: strings.Repeat("x", 80)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crClient := crfake.NewClientBuilder().WithScheme(s).Build()
			snap, err := createPodSnapshot(context.Background(), crClient, "default", tt.snapName, "my-pod", podUID, []string{"main"})
			require.NoError(t, err)
			assert.Equal(t, podUID, snap.Spec.Source.PodRef.UID, "source pod UID must be pinned")
			assert.Equal(t, "my-pod", snap.Spec.Source.PodRef.Name)
			assert.Equal(t, []string{"main"}, snap.Spec.Source.PodRef.Containers, "target container must be set on the PodSnapshot")
		})
	}
}

// TestPodSnapshotAlreadyExists verifies that createPodSnapshot returns a clear error when
// a PodSnapshot with the derived name already exists.
func TestPodSnapshotAlreadyExists(t *testing.T) {
	s := snapshotScheme(t)
	existing := &nvidiacomv1alpha1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snap", Namespace: "default"},
		Spec: nvidiacomv1alpha1.PodSnapshotSpec{
			Source: nvidiacomv1alpha1.PodSnapshotSource{
				PodRef: nvidiacomv1alpha1.PodReference{Name: "other-pod"},
			},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()

	_, err := createPodSnapshot(context.Background(), crClient, "default", "my-snap", "my-pod", "uid-1", []string{"main"})
	require.Error(t, err)
	assert.True(t, apierrors.IsAlreadyExists(err) || strings.Contains(err.Error(), "already exists"),
		"error should report AlreadyExists: %v", err)
}

// TestWaitForPodSnapshotReady verifies that waitForPodSnapshot returns the PodSnapshot when
// its Ready condition is True.
func TestWaitForPodSnapshotReady(t *testing.T) {
	s := snapshotScheme(t)
	content := "my-content"
	snap := &nvidiacomv1alpha1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1", Namespace: "default"},
		Spec: nvidiacomv1alpha1.PodSnapshotSpec{
			Source: nvidiacomv1alpha1.PodSnapshotSource{
				PodRef: nvidiacomv1alpha1.PodReference{Name: "pod-1"},
			},
		},
		Status: nvidiacomv1alpha1.PodSnapshotStatus{
			BoundPodSnapshotContentName: &content,
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             "Captured",
					Message:            "checkpoint captured",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
	crClient := crfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(snap).
		Build()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := waitForPodSnapshot(ctx, crClient, "default", "snap-1")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, nvidiacomv1alpha1.IsPodSnapshotSucceeded(result))
	assert.NotNil(t, result.Status.BoundPodSnapshotContentName)
}

// TestWaitForPodSnapshotFailed verifies that waitForPodSnapshot returns an error surfacing the
// Failed condition's Reason and Message when the PodSnapshot fails.
func TestWaitForPodSnapshotFailed(t *testing.T) {
	s := snapshotScheme(t)
	snap := &nvidiacomv1alpha1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-fail", Namespace: "default"},
		Spec: nvidiacomv1alpha1.PodSnapshotSpec{
			Source: nvidiacomv1alpha1.PodSnapshotSource{
				PodRef: nvidiacomv1alpha1.PodReference{Name: "pod-1"},
			},
		},
		Status: nvidiacomv1alpha1.PodSnapshotStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "Failed",
					Status:             metav1.ConditionTrue,
					Reason:             "AgentError",
					Message:            "CRIU dump failed: no such process",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
	crClient := crfake.NewClientBuilder().
		WithScheme(s).
		WithObjects(snap).
		Build()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := waitForPodSnapshot(ctx, crClient, "default", "snap-fail")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AgentError", "error must surface the Failed condition Reason")
	assert.Contains(t, err.Error(), "CRIU dump failed: no such process", "error must surface the Failed condition Message")
}

// TestWaitForSourcePodTimeout verifies that waitForSourcePod returns an actionable error when
// the overall context expires before the pod appears.
func TestWaitForSourcePodTimeout(t *testing.T) {
	// Use a fake clientset with no pods — the pod will never appear.
	clientset := k8sfake.NewClientset()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := waitForSourcePod(ctx, clientset, "default", "my-job", "job-uid-1")
	require.Error(t, err)
	// Must mention the job so the caller knows which job is stuck.
	assert.Contains(t, err.Error(), "my-job", "timeout error must name the job")
}

// TestWaitForSourcePodUnscheduled verifies that waitForSourcePod keeps waiting when the pod
// exists but has not yet been scheduled (Spec.NodeName == ""), then times out with an actionable
// error rather than returning the unscheduled pod.
func TestWaitForSourcePodUnscheduled(t *testing.T) {
	controllerTrue := true
	jobUID := types.UID("job-uid-2")
	unscheduledPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-job-pod",
			Namespace: "default",
			Labels:    map[string]string{"batch.kubernetes.io/job-name": "my-job"},
			OwnerReferences: []metav1.OwnerReference{
				{UID: jobUID, Controller: &controllerTrue},
			},
		},
		Spec: corev1.PodSpec{
			// NodeName is empty — not yet scheduled.
			Containers: []corev1.Container{{Name: "main", Image: "test"}},
		},
	}
	clientset := k8sfake.NewClientset(unscheduledPod)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := waitForSourcePod(ctx, clientset, "default", "my-job", jobUID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "my-job", "timeout error must name the job")
}

// TestWaitForPodSnapshotContextDeadline verifies that waitForPodSnapshot propagates a context
// deadline exceeded error when the PodSnapshot never reaches a terminal state.
func TestWaitForPodSnapshotContextDeadline(t *testing.T) {
	s := snapshotScheme(t)
	// Non-terminal PodSnapshot: no Ready or Failed condition.
	snap := &nvidiacomv1alpha1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-pending", Namespace: "default"},
		Spec: nvidiacomv1alpha1.PodSnapshotSpec{
			Source: nvidiacomv1alpha1.PodSnapshotSource{
				PodRef: nvidiacomv1alpha1.PodReference{Name: "pod-1"},
			},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(s).WithObjects(snap).Build()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := waitForPodSnapshot(ctx, crClient, "default", "snap-pending")
	require.Error(t, err)
}
