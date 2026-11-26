//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	"github.com/ai-dynamo/snapshot/api/storage"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

func TestPodSnapshotBindsThroughItsStoreAgainstAnAPIServer(t *testing.T) {
	var defs []*apiextensionsv1.CustomResourceDefinition
	for _, def := range []string{
		crds.PodSnapshotCRD(), crds.PodSnapshotContentCRD(), crds.SnapshotJobCRD(), crds.SnapshotStoreCRD(),
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
	for _, namespace := range []string{"snapshot", "workload"} {
		require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}:    {Namespaces: map[string]cache.Config{"snapshot": {}}},
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{"snapshot": {}}},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, mgr.Add(&DefaultSnapshotStoreCreator{
		Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Name: "default",
		Config: storage.Config{PVC: &storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: "/"}},
	}))
	require.NoError(t, (&SnapshotStoreReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Recorder: record.NewFakeRecorder(100), Prober: &gatedProber{},
		Namespace: "snapshot", ClusterID: "cluster-a", DefaultStoreName: "default", BindingEnabled: true,
		MountPath: func(string) (string, bool) { return t.TempDir(), true },
	}).SetupWithManager(mgr))
	require.NoError(t, (&PodSnapshotReconciler{
		Client: mgr.GetClient(), NonCacheReadClient: mgr.GetAPIReader(), Recorder: record.NewFakeRecorder(100),
		BindNewContents: true, DefaultStoreName: "default",
	}).SetupWithManager(mgr))
	managerCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go func() { _ = mgr.Start(managerCtx) }()

	readyReason := func(name string) string {
		store := &snapshotv1alpha1.SnapshotStore{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name}, store); err != nil {
			return "missing"
		}
		if cond := meta.FindStatusCondition(store.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady); cond != nil {
			return cond.Reason
		}
		return ""
	}
	newSnapshot := func(name, podName, storeName string) *snapshotv1alpha1.PodSnapshot {
		require.NoError(t, cl.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "workload", Name: podName},
			Spec: corev1.PodSpec{
				NodeName:   "node-a",
				Containers: []corev1.Container{{Name: "main", Image: "workload:latest"}},
			},
		}))
		snap := &snapshotv1alpha1.PodSnapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: "workload", Name: name},
			Spec: snapshotv1alpha1.PodSnapshotSpec{
				Source:            snapshotv1alpha1.PodSnapshotSource{PodRef: snapshotv1alpha1.PodReference{Name: podName, Containers: []string{"main"}}},
				SnapshotStoreName: storeName,
			},
		}
		require.NoError(t, cl.Create(ctx, snap))
		return snap
	}
	boundContent := func(snap *snapshotv1alpha1.PodSnapshot) *snapshotv1alpha1.PodSnapshotContent {
		current := &snapshotv1alpha1.PodSnapshot{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(snap), current); err != nil || current.Status.BoundPodSnapshotContentName == nil {
			return nil
		}
		content := &snapshotv1alpha1.PodSnapshotContent{}
		if err := cl.Get(ctx, types.NamespacedName{Name: *current.Status.BoundPodSnapshotContentName}, content); err != nil {
			return nil
		}
		return content
	}
	pendingReason := func(snap *snapshotv1alpha1.PodSnapshot) string {
		current := &snapshotv1alpha1.PodSnapshot{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(snap), current); err != nil {
			return ""
		}
		if cond := meta.FindStatusCondition(current.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady); cond != nil {
			return cond.Reason
		}
		return ""
	}

	t.Run("the default store is created and ready", func(t *testing.T) {
		require.Eventually(t, func() bool { return readyReason("default") == ReasonStoreReachable }, storeEventually, storeTick)
	})

	t.Run("a request that names no store binds through the default store", func(t *testing.T) {
		snap := newSnapshot("unnamed", "unnamed-pod", "")
		require.Eventually(t, func() bool { return boundContent(snap) != nil }, storeEventually, storeTick)

		store := &snapshotv1alpha1.SnapshotStore{}
		require.NoError(t, cl.Get(ctx, types.NamespacedName{Name: "default"}, store))
		content := boundContent(snap)
		require.NotNil(t, content.Spec.Storage)
		assert.Equal(t, store.Status.StoreID, content.Spec.Storage.StoreID)
		assert.Equal(t, "default", content.Spec.Storage.SnapshotStoreName)
		assert.Equal(t, snapshotv1alpha1.ReclaimPolicyDelete, content.Spec.Storage.ReclaimPolicy)
		assert.Equal(t, "default", content.Labels[snapshotv1alpha1.SnapshotStoreLabel])
	})

	t.Run("a request waits for a store that does not exist yet", func(t *testing.T) {
		snap := newSnapshot("named", "named-pod", "gold")
		require.Eventually(t, func() bool { return pendingReason(snap) == ReasonStoreNotFound }, storeEventually, storeTick)
		assert.Nil(t, boundContent(snap))

		require.NoError(t, cl.Create(ctx, &snapshotv1alpha1.SnapshotStore{
			ObjectMeta: metav1.ObjectMeta{Name: "gold"},
			Spec: snapshotv1alpha1.SnapshotStoreSpec{
				ReclaimPolicy: snapshotv1alpha1.ReclaimPolicyRetain,
				PVC:           &snapshotv1alpha1.PVCStoreSpec{ClaimName: "snapshot-pvc", SubPath: "/gold"},
			},
		}))
		require.Eventually(t, func() bool { return boundContent(snap) != nil }, storeEventually, storeTick)
		content := boundContent(snap)
		assert.Equal(t, "gold", content.Spec.Storage.SnapshotStoreName)
		assert.Equal(t, snapshotv1alpha1.ReclaimPolicyRetain, content.Spec.Storage.ReclaimPolicy)
	})
}
