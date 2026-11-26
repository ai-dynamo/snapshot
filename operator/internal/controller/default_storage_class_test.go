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

func newDefaultClassCreator(t *testing.T, cfg storage.Config, objects ...client.Object) (*DefaultStorageClassCreator, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &DefaultStorageClassCreator{Client: c, Reader: c, Name: "default", Config: cfg}, c
}

func classNames(t *testing.T, c client.Client) []string {
	t.Helper()
	classes := &snapshotv1alpha1.SnapshotStorageClassList{}
	require.NoError(t, c.List(t.Context(), classes))
	var names []string
	for _, class := range classes.Items {
		names = append(names, class.Name)
	}
	return names
}

func TestDefaultClassIsCreatedFromAPVCStore(t *testing.T) {
	creator, c := newDefaultClassCreator(t, storage.Config{PVC: &storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: "/"}})

	require.NoError(t, creator.Ensure(t.Context()))

	class := &snapshotv1alpha1.SnapshotStorageClass{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "default"}, class))
	require.NotNil(t, class.Spec.PVC)
	assert.Equal(t, "snapshot-pvc", class.Spec.PVC.ClaimName)
	assert.Empty(t, class.Spec.PVC.BasePath)
	assert.Equal(t, snapshotv1alpha1.ReclaimPolicyDelete, class.Spec.ReclaimPolicy)
	want, err := storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: "/"}.StoreID()
	require.NoError(t, err)
	got, err := classStoreID(class, "snapshot")
	require.NoError(t, err)
	assert.Equal(t, want, got, "the class has the store ID that bound content already carries")
}

func TestDefaultClassIsCreatedFromAnS3StoreWithItsAccessSettings(t *testing.T) {
	cfg := storage.Config{
		S3: &storage.S3{Endpoint: "https://s3.example.com", Region: "region-1", Bucket: "snapshots", Prefix: "team-a"},
		S3Access: &storage.S3Access{
			CredentialsSecretRef: &storage.KeyRef{Name: "s3-credentials"},
			Addressing:           "path",
			AllowHTTP:            true,
			CABundleConfigMapRef: &storage.KeyRef{Name: "s3-ca", Key: "bundle.pem"},
		},
	}
	creator, c := newDefaultClassCreator(t, cfg)

	require.NoError(t, creator.Ensure(t.Context()))

	class := &snapshotv1alpha1.SnapshotStorageClass{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "default"}, class))
	s3 := class.Spec.S3
	require.NotNil(t, s3)
	assert.Equal(t, "team-a", s3.Prefix)
	assert.Equal(t, "path", s3.Addressing)
	assert.True(t, s3.AllowHTTP)
	assert.Equal(t, &snapshotv1alpha1.SecretKeyReference{Name: "s3-credentials", Key: "credentials"}, s3.CredentialsSecretRef)
	assert.Equal(t, &snapshotv1alpha1.ConfigMapKeyReference{Name: "s3-ca", Key: "bundle.pem"}, s3.CABundleConfigMapRef)
	want, err := cfg.S3.StoreID()
	require.NoError(t, err)
	got, err := classStoreID(class, "snapshot")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestDefaultClassWithoutASecretUsesTheWorkloadIdentity(t *testing.T) {
	creator, c := newDefaultClassCreator(t, storage.Config{
		S3: &storage.S3{Endpoint: "https://s3.example.com", Region: "region-1", Bucket: "snapshots"},
	})

	require.NoError(t, creator.Ensure(t.Context()))

	class := &snapshotv1alpha1.SnapshotStorageClass{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "default"}, class))
	assert.Nil(t, class.Spec.S3.CredentialsSecretRef)
	assert.Nil(t, class.Spec.S3.CABundleConfigMapRef)
}

func TestDefaultClassIsNotCreatedWhenAnyClassExists(t *testing.T) {
	existing := &snapshotv1alpha1.SnapshotStorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gold"}}
	creator, c := newDefaultClassCreator(t, storage.Config{PVC: &storage.PVC{ClaimName: "snapshot-pvc"}}, existing)

	require.NoError(t, creator.Ensure(t.Context()))

	assert.Equal(t, []string{"gold"}, classNames(t, c), "a deleted default is not brought back while another class exists")
}

func TestDefaultClassIsLeftAloneOnceItExists(t *testing.T) {
	existing := &snapshotv1alpha1.SnapshotStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       snapshotv1alpha1.SnapshotStorageClassSpec{ReclaimPolicy: snapshotv1alpha1.ReclaimPolicyRetain},
	}
	creator, c := newDefaultClassCreator(t, storage.Config{PVC: &storage.PVC{ClaimName: "snapshot-pvc"}}, existing)

	require.NoError(t, creator.Ensure(t.Context()))

	class := &snapshotv1alpha1.SnapshotStorageClass{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "default"}, class))
	assert.Equal(t, snapshotv1alpha1.ReclaimPolicyRetain, class.Spec.ReclaimPolicy, "an edit is not reverted")
}

func TestNoDefaultClassIsCreatedWithoutAStore(t *testing.T) {
	creator, c := newDefaultClassCreator(t, storage.Config{})

	require.NoError(t, creator.Ensure(t.Context()))

	assert.Empty(t, classNames(t, c))
}

func TestEnsureIsRepeatable(t *testing.T) {
	creator, c := newDefaultClassCreator(t, storage.Config{PVC: &storage.PVC{ClaimName: "snapshot-pvc"}})

	require.NoError(t, creator.Ensure(t.Context()))
	require.NoError(t, creator.Ensure(t.Context()))

	assert.Equal(t, []string{"default"}, classNames(t, c))
}
