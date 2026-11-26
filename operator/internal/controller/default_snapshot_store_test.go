// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ai-dynamo/snapshot/api/storage"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func newDefaultStoreCreator(t *testing.T, cfg storage.Config, objects ...client.Object) (*DefaultSnapshotStoreCreator, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &DefaultSnapshotStoreCreator{Client: c, Reader: c, Name: "default", Config: cfg}, c
}

func storeNames(t *testing.T, c client.Client) []string {
	t.Helper()
	stores := &snapshotv1alpha1.SnapshotStoreList{}
	require.NoError(t, c.List(t.Context(), stores))
	var names []string
	for _, store := range stores.Items {
		names = append(names, store.Name)
	}
	return names
}

func TestDefaultStoreIsCreatedFromAPVCStore(t *testing.T) {
	creator, c := newDefaultStoreCreator(t, storage.Config{PVC: &storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: "/"}})

	require.NoError(t, creator.Ensure(t.Context()))

	store := &snapshotv1alpha1.SnapshotStore{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "default"}, store))
	require.NotNil(t, store.Spec.PVC)
	assert.Equal(t, "snapshot-pvc", store.Spec.PVC.ClaimName)
	assert.Empty(t, store.Spec.PVC.SubPath)
	assert.Equal(t, snapshotv1alpha1.ReclaimPolicyDelete, store.Spec.ReclaimPolicy)
	want, err := storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: "/"}.StoreID()
	require.NoError(t, err)
	got, err := storeIDOf(store, "snapshot")
	require.NoError(t, err)
	assert.Equal(t, want, got, "the store has the store ID that bound content already carries")
}

func TestDefaultStoreIsCreatedFromAnS3StoreWithItsAccessSettings(t *testing.T) {
	cfg := storage.Config{
		S3: &storage.S3{Endpoint: "https://s3.example.com", Region: "region-1", Bucket: "snapshots", Prefix: "team-a"},
		S3Access: &storage.S3Access{
			CredentialsSecretRef: &storage.KeyRef{Name: "s3-credentials"},
			Addressing:           "path",
			AllowHTTP:            true,
			CABundleConfigMapRef: &storage.KeyRef{Name: "s3-ca", Key: "bundle.pem"},
		},
	}
	creator, c := newDefaultStoreCreator(t, cfg)

	require.NoError(t, creator.Ensure(t.Context()))

	store := &snapshotv1alpha1.SnapshotStore{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "default"}, store))
	s3 := store.Spec.S3
	require.NotNil(t, s3)
	assert.Equal(t, "team-a", s3.Prefix)
	assert.Equal(t, "path", s3.Addressing)
	assert.True(t, s3.AllowHTTP)
	assert.Equal(t, &snapshotv1alpha1.SecretKeyReference{Name: "s3-credentials", Key: "credentials"}, s3.CredentialsSecretRef)
	assert.Equal(t, &snapshotv1alpha1.ConfigMapKeyReference{Name: "s3-ca", Key: "bundle.pem"}, s3.CABundleConfigMapRef)
	want, err := cfg.S3.StoreID()
	require.NoError(t, err)
	got, err := storeIDOf(store, "snapshot")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestDefaultStoreWithoutASecretUsesTheWorkloadIdentity(t *testing.T) {
	creator, c := newDefaultStoreCreator(t, storage.Config{
		S3: &storage.S3{Endpoint: "https://s3.example.com", Region: "region-1", Bucket: "snapshots"},
	})

	require.NoError(t, creator.Ensure(t.Context()))

	store := &snapshotv1alpha1.SnapshotStore{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "default"}, store))
	assert.Nil(t, store.Spec.S3.CredentialsSecretRef)
	assert.Nil(t, store.Spec.S3.CABundleConfigMapRef)
}

func TestDefaultStoreIsNotCreatedWhenAnyStoreExists(t *testing.T) {
	existing := &snapshotv1alpha1.SnapshotStore{ObjectMeta: metav1.ObjectMeta{Name: "gold"}}
	creator, c := newDefaultStoreCreator(t, storage.Config{PVC: &storage.PVC{ClaimName: "snapshot-pvc"}}, existing)

	require.NoError(t, creator.Ensure(t.Context()))

	assert.Equal(t, []string{"gold"}, storeNames(t, c), "a deleted default is not brought back while another store exists")
}

func TestDefaultStoreIsLeftAloneOnceItExists(t *testing.T) {
	existing := &snapshotv1alpha1.SnapshotStore{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       snapshotv1alpha1.SnapshotStoreSpec{ReclaimPolicy: snapshotv1alpha1.ReclaimPolicyRetain},
	}
	creator, c := newDefaultStoreCreator(t, storage.Config{PVC: &storage.PVC{ClaimName: "snapshot-pvc"}}, existing)

	require.NoError(t, creator.Ensure(t.Context()))

	store := &snapshotv1alpha1.SnapshotStore{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "default"}, store))
	assert.Equal(t, snapshotv1alpha1.ReclaimPolicyRetain, store.Spec.ReclaimPolicy, "an edit is not reverted")
}

func TestNoDefaultStoreIsCreatedWithoutAStore(t *testing.T) {
	creator, c := newDefaultStoreCreator(t, storage.Config{})

	require.NoError(t, creator.Ensure(t.Context()))

	assert.Empty(t, storeNames(t, c))
}

func TestEnsureIsRepeatable(t *testing.T) {
	creator, c := newDefaultStoreCreator(t, storage.Config{PVC: &storage.PVC{ClaimName: "snapshot-pvc"}})

	require.NoError(t, creator.Ensure(t.Context()))
	require.NoError(t, creator.Ensure(t.Context()))

	assert.Equal(t, []string{"default"}, storeNames(t, c))
}
