// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/maintenance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func snapshotContentTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1alpha1.AddToScheme(scheme))
	return scheme
}

type deleteContentCall struct {
	namespace, name string
	uid             types.UID
}

type fakeEnqueuer struct {
	calls []deleteContentCall
}

func (f *fakeEnqueuer) EnqueueDeleteContent(namespace, name string, uid types.UID) {
	f.calls = append(f.calls, deleteContentCall{namespace: namespace, name: name, uid: uid})
}

func TestSnapshotContentReconcilerAddsFinalizer(t *testing.T) {
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-1"), ResourceVersion: "1", Finalizers: []string{"example.com/other"},
	}}
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(snapshotContentTestScheme(t)).WithObjects(content).Build()
	enqueuer := &fakeEnqueuer{}
	_, err := reconcileSnapshotContent(context.Background(), kubeClient, enqueuer, ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.NoError(t, err)

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.ElementsMatch(t, []string{"example.com/other", maintenance.PodSnapshotContentArtifactCleanupFinalizer}, current.Finalizers)
	assert.Empty(t, enqueuer.calls)
}

func TestSnapshotContentReconcilerNoopWhenFinalizerAlreadyPresent(t *testing.T) {
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-2"), ResourceVersion: "1",
		Finalizers: []string{maintenance.PodSnapshotContentArtifactCleanupFinalizer},
	}}
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(snapshotContentTestScheme(t)).WithObjects(content).Build()
	enqueuer := &fakeEnqueuer{}
	_, err := reconcileSnapshotContent(context.Background(), kubeClient, enqueuer, ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.NoError(t, err)
	assert.Empty(t, enqueuer.calls)
}

func TestSnapshotContentReconcilerEnqueuesDeleteContentWhenFinalizerPresent(t *testing.T) {
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", Namespace: "ns", UID: types.UID("uid-3"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{"example.com/other", maintenance.PodSnapshotContentArtifactCleanupFinalizer},
	}}
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(snapshotContentTestScheme(t)).WithObjects(content).Build()
	enqueuer := &fakeEnqueuer{}
	_, err := reconcileSnapshotContent(context.Background(), kubeClient, enqueuer, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "ns", Name: content.Name}})
	require.NoError(t, err)

	require.Len(t, enqueuer.calls, 1)
	assert.Equal(t, deleteContentCall{namespace: "ns", name: "content", uid: types.UID("uid-3")}, enqueuer.calls[0])

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: content.Name}, current))
	assert.Contains(t, current.Finalizers, maintenance.PodSnapshotContentArtifactCleanupFinalizer)
}

func TestSnapshotContentReconcilerNoopWhenDeletingWithoutFinalizer(t *testing.T) {
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-4"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{"example.com/other"},
	}}
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(snapshotContentTestScheme(t)).WithObjects(content).Build()
	enqueuer := &fakeEnqueuer{}
	_, err := reconcileSnapshotContent(context.Background(), kubeClient, enqueuer, ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.NoError(t, err)
	assert.Empty(t, enqueuer.calls)
}

func TestSnapshotContentReconcilerIgnoresMissingContent(t *testing.T) {
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(snapshotContentTestScheme(t)).Build()
	enqueuer := &fakeEnqueuer{}
	_, err := reconcileSnapshotContent(context.Background(), kubeClient, enqueuer, ctrl.Request{NamespacedName: client.ObjectKey{Name: "missing"}})
	require.NoError(t, err)
	assert.Empty(t, enqueuer.calls)
	require.False(t, apierrors.IsNotFound(err)) // IgnoreNotFound already applied
}
