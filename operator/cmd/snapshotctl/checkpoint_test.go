//go:build linux

// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

func unansweredRequest() error {
	return &url.Error{Op: "Get", URL: "https://kubernetes.example/api", Err: syscall.ECONNREFUSED}
}

func readyPodSnapshot(name string) *nvidiacomv1alpha1.PodSnapshot {
	return &nvidiacomv1alpha1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: nvidiacomv1alpha1.PodSnapshotSpec{
			Source: nvidiacomv1alpha1.PodSnapshotSource{PodRef: nvidiacomv1alpha1.PodReference{Name: "pod-1"}},
		},
		Status: nvidiacomv1alpha1.PodSnapshotStatus{
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "Captured", LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// podSnapshotClient fails the first failures Get calls with err, then answers.
func podSnapshotClient(t *testing.T, snap *nvidiacomv1alpha1.PodSnapshot, failures int, err error) (client.Client, *int) {
	t.Helper()
	calls := 0
	crClient := crfake.NewClientBuilder().WithScheme(snapshotScheme(t)).WithObjects(snap).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				calls++
				if calls <= failures {
					return err
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	return crClient, &calls
}

func TestRetriedAPIErrorsRetry(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"connection refused", unansweredRequest(), true},
		{"request deadline", context.DeadlineExceeded, true},
		{"service unavailable", apierrors.NewServiceUnavailable("starting"), true},
		{"internal error", apierrors.NewInternalError(errors.New("etcd leader changed")), true},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), true},
		{"server timeout", apierrors.NewServerTimeout(pods, "list", 1), true},
		{"forbidden", apierrors.NewForbidden(pods, "", errors.New("no access")), false},
		{"unauthorized", apierrors.NewUnauthorized("token expired"), false},
		{"bad request", apierrors.NewBadRequest("malformed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var retried retriedAPIErrors
			assert.Equal(t, tc.retry, retried.retry(context.Background(), tc.err))
		})
	}
}

func TestWaitForPodSnapshotOutlastsUnansweredRequest(t *testing.T) {
	crClient, calls := podSnapshotClient(t, readyPodSnapshot("snap-1"), 1, unansweredRequest())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := waitForPodSnapshot(ctx, crClient, "default", "snap-1")
	require.NoError(t, err)
	assert.True(t, nvidiacomv1alpha1.IsPodSnapshotSucceeded(result))
	assert.Equal(t, 2, *calls)
}

func TestWaitForPodSnapshotTimeoutNamesLastAPIError(t *testing.T) {
	crClient, _ := podSnapshotClient(t, readyPodSnapshot("snap-1"), 1000, unansweredRequest())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := waitForPodSnapshot(ctx, crClient, "default", "snap-1")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "connection refused")
}

// A refusal is an answer: waiting cannot change it, so the wait must end at once.
func TestWaitForPodSnapshotStopsOnForbidden(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "nvidia.com", Resource: "podsnapshots"}, "snap-1", errors.New("no access"))
	crClient, calls := podSnapshotClient(t, readyPodSnapshot("snap-1"), 1000, forbidden)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := waitForPodSnapshot(ctx, crClient, "default", "snap-1")
	require.True(t, apierrors.IsForbidden(err), "got %v", err)
	assert.Equal(t, 1, *calls)
}

func TestWaitForSourcePodOutlastsUnansweredRequest(t *testing.T) {
	controllerTrue := true
	jobUID := types.UID("job-uid-3")
	clientset := k8sfake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-job-pod", Namespace: "default",
			Labels:          map[string]string{"batch.kubernetes.io/job-name": "my-job"},
			OwnerReferences: []metav1.OwnerReference{{UID: jobUID, Controller: &controllerTrue}},
		},
		Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "main", Image: "test"}}},
	})
	calls := 0
	clientset.PrependReactor("list", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, unansweredRequest()
		}
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pod, err := waitForSourcePod(ctx, clientset, "default", "my-job", jobUID)
	require.NoError(t, err)
	assert.Equal(t, "my-job-pod", pod.Name)
	assert.Equal(t, 2, calls)
}
