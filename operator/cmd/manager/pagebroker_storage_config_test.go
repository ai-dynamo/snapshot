// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ai-dynamo/snapshot/api/storage"
)

func fakeClient(t *testing.T) *fake.ClientBuilder {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme)
}

func TestEnsurePageBrokerS3ConfigCreates(t *testing.T) {
	c := fakeClient(t).Build()
	s3 := storage.S3{Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "snapshot-bucket", Prefix: "containers"}
	require.NoError(t, ensurePageBrokerS3Config(t.Context(), c, "snapshot", "snapshot-pagebroker-storage-config", s3, "store-v1-abc"))

	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "snapshot", Name: "snapshot-pagebroker-storage-config"}, &cm))
	var decoded pageBrokerS3Config
	require.NoError(t, json.Unmarshal([]byte(cm.Data[PageBrokerS3ConfigKey]), &decoded))
	assert.Equal(t, pageBrokerS3Config{
		StoreID: "store-v1-abc", Endpoint: "https://s3.example.com", Region: "us-east-1",
		Bucket: "snapshot-bucket", Prefix: "containers",
	}, decoded)
}

func TestEnsurePageBrokerS3ConfigUpdatesExisting(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "snapshot", Name: "snapshot-pagebroker-storage-config"},
		Data:       map[string]string{PageBrokerS3ConfigKey: `{"storeId":"stale"}`},
	}
	c := fakeClient(t).WithObjects(existing).Build()
	s3 := storage.S3{Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "snapshot-bucket"}
	require.NoError(t, ensurePageBrokerS3Config(t.Context(), c, "snapshot", "snapshot-pagebroker-storage-config", s3, "store-v1-fresh"))

	var cm corev1.ConfigMap
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "snapshot", Name: "snapshot-pagebroker-storage-config"}, &cm))
	assert.Contains(t, cm.Data[PageBrokerS3ConfigKey], `"storeId":"store-v1-fresh"`)
}

func TestEnsurePageBrokerS3ConfigIsIdempotent(t *testing.T) {
	c := fakeClient(t).Build()
	s3 := storage.S3{Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "snapshot-bucket"}
	for range 2 {
		require.NoError(t, ensurePageBrokerS3Config(t.Context(), c, "snapshot", "snapshot-pagebroker-storage-config", s3, "store-v1-abc"))
	}
}
