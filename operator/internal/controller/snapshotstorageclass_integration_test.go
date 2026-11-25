//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
	"github.com/ai-dynamo/snapshot/operator/internal/storageprobe"
)

const (
	classEventually = 20 * time.Second
	classTick       = 100 * time.Millisecond
)

func TestSnapshotStorageClassControllerAgainstAnAPIServer(t *testing.T) {
	var defs []*apiextensionsv1.CustomResourceDefinition
	for _, def := range []string{
		crds.PodSnapshotCRD(), crds.PodSnapshotContentCRD(), crds.SnapshotJobCRD(), crds.SnapshotStorageClassCRD(),
	} {
		var crd apiextensionsv1.CustomResourceDefinition
		require.NoError(t, yaml.Unmarshal([]byte(def), &crd))
		defs = append(defs, &crd)
	}
	env := &envtest.Environment{CRDs: defs}
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	cfg, err := env.Start()
	require.NoError(t, err)

	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := t.Context()
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "snapshot"}}))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}:    {Namespaces: map[string]cache.Config{"snapshot": {}}},
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{"snapshot": {}}},
		}},
	})
	require.NoError(t, err)
	prober := &gatedProber{}
	require.NoError(t, (&SnapshotStorageClassReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Recorder: record.NewFakeRecorder(100), Prober: prober,
		Namespace: "snapshot", ClusterID: "cluster-a", DefaultClassName: "default", BindingEnabled: true,
		MountPath: func(string) (string, bool) { return t.TempDir(), true },
	}).SetupWithManager(mgr))
	managerCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go func() { _ = mgr.Start(managerCtx) }()

	readyReason := func(name string) string {
		class := &snapshotv1alpha1.SnapshotStorageClass{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, class); err != nil {
			return "missing"
		}
		if cond := meta.FindStatusCondition(class.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady); cond != nil {
			return cond.Reason
		}
		return ""
	}
	create := func(name, prefix string) *snapshotv1alpha1.SnapshotStorageClass {
		class := &snapshotv1alpha1.SnapshotStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: snapshotv1alpha1.SnapshotStorageClassSpec{S3: &snapshotv1alpha1.S3StoreSpec{
				Endpoint: "https://s3.example.com", Region: "region-1", Bucket: "snapshots", Prefix: prefix,
			}},
		}
		require.NoError(t, cl.Create(ctx, class))
		return class
	}

	t.Run("a class becomes ready and gets its finalizer", func(t *testing.T) {
		create("default", "team-a")
		require.Eventually(t, func() bool { return readyReason("default") == ReasonStoreReachable }, classEventually, classTick)
		class := &snapshotv1alpha1.SnapshotStorageClass{}
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "default"}, class))
		assert.Contains(t, class.Finalizers, StorageClassProtectionFinalizer)
		assert.Regexp(t, `^store-v1-[0-9a-f]{64}$`, class.Status.StoreID)
	})

	t.Run("a duplicate is refused and recovers when the first class goes", func(t *testing.T) {
		create("z-copy", "team-a")
		require.Eventually(t, func() bool { return readyReason("z-copy") == ReasonDuplicateStore }, classEventually, classTick)

		require.NoError(t, cl.Delete(ctx, &snapshotv1alpha1.SnapshotStorageClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
		require.Eventually(t, func() bool { return readyReason("default") == "missing" }, classEventually, classTick)
		require.Eventually(t, func() bool { return readyReason("z-copy") == ReasonStoreReachable }, classEventually, classTick)
	})

	t.Run("a Secret that appears is picked up", func(t *testing.T) {
		class := &snapshotv1alpha1.SnapshotStorageClass{
			ObjectMeta: metav1.ObjectMeta{Name: "keyed"},
			Spec: snapshotv1alpha1.SnapshotStorageClassSpec{S3: &snapshotv1alpha1.S3StoreSpec{
				Endpoint: "https://s3.example.com", Region: "region-1", Bucket: "snapshots", Prefix: "keyed",
				CredentialsSecretRef: &snapshotv1alpha1.SecretKeyReference{Name: "s3-credentials", Key: "credentials"},
			}},
		}
		require.NoError(t, cl.Create(ctx, class))
		require.Eventually(t, func() bool { return readyReason("keyed") == ReasonCredentialsUnavailable }, classEventually, classTick)

		require.NoError(t, cl.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "snapshot", Name: "s3-credentials"},
			Data:       map[string][]byte{"credentials": []byte("[default]\naws_access_key_id = A\naws_secret_access_key = B\n")},
		}))
		require.Eventually(t, func() bool { return readyReason("keyed") == ReasonStoreReachable }, classEventually, classTick)
	})

	t.Run("a class is kept while content uses it", func(t *testing.T) {
		class := &snapshotv1alpha1.SnapshotStorageClass{}
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "z-copy"}, class))
		content := contentFor("uses-copy", "z-copy", class.Status.StoreID)
		content.Spec.PodSnapshotRef = snapshotv1alpha1.PodSnapshotReference{Namespace: "workload", Name: "snapshot"}
		content.Spec.Source = snapshotv1alpha1.PodSnapshotContentSource{
			NodeName: "node-a", PodRef: snapshotv1alpha1.PodReference{Name: "source", Containers: []string{"main"}},
		}
		content.Spec.Storage.ReclaimPolicy = snapshotv1alpha1.ReclaimPolicyDelete
		require.NoError(t, cl.Create(ctx, content))

		require.NoError(t, cl.Delete(ctx, class))
		require.Eventually(t, func() bool { return readyReason("z-copy") == ReasonTerminating }, classEventually, classTick)
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "z-copy"}, class))
		assert.False(t, class.DeletionTimestamp.IsZero())

		require.NoError(t, cl.Delete(ctx, content))
		require.Eventually(t, func() bool {
			err := cl.Get(ctx, types.NamespacedName{Name: "z-copy"}, &snapshotv1alpha1.SnapshotStorageClass{})
			return apierrors.IsNotFound(err)
		}, 2*classEventually, classTick)
	})
}

type gatedProber struct{}

func (gatedProber) Check(context.Context, storageprobe.Target) error { return nil }
