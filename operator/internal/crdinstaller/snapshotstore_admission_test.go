//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/ai-dynamo/snapshot/api/storage"
	snapshotv1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

func startStoreEnvironment(t *testing.T) client.Client {
	t.Helper()
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
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	require.NoError(t, cl.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload"}}))
	return cl
}

func pvcStore(name string) *snapshotv1.SnapshotStore {
	return &snapshotv1.SnapshotStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: snapshotv1.SnapshotStoreSpec{
			PVC: &snapshotv1.PVCStoreSpec{ClaimName: "snapshot-pvc", SubPath: "/models"},
		},
	}
}

func s3Store(name string) *snapshotv1.SnapshotStore {
	return &snapshotv1.SnapshotStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: snapshotv1.SnapshotStoreSpec{
			S3: &snapshotv1.S3StoreSpec{
				Endpoint: "https://s3.example.com", Region: "region-1", Bucket: "snapshots", Prefix: "team-a",
			},
		},
	}
}

func isInvalid(err error) bool { return apierrors.IsInvalid(err) }

func TestSnapshotStoreCreateAdmission(t *testing.T) {
	cl := startStoreEnvironment(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name   string
		build  func(name string) *snapshotv1.SnapshotStore
		change func(*snapshotv1.SnapshotStore)
		valid  bool
	}{
		{"pvc", pvcStore, nil, true},
		{"s3", s3Store, nil, true},
		{"pvc-root", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.SubPath = "" }, true},
		{"pvc-relative-subpath", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.SubPath = "models" }, false},
		{"pvc-claims-segment", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.SubPath = "/.claims/team-a" }, false},
		{"s3-no-prefix", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Prefix = "" }, true},
		{"no-block", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC = nil }, false},
		{"both-blocks", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.S3 = s3Store("x").Spec.S3 }, false},
		{"reclaim-retain", pvcStore, func(c *snapshotv1.SnapshotStore) {
			c.Spec.ReclaimPolicy = snapshotv1.ReclaimPolicyRetain
		}, true},
		{"reclaim-unknown", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.ReclaimPolicy = "Archive" }, false},
		{"empty-claim", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.ClaimName = "" }, false},
		{"pvc-artifacts-segment", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.SubPath = "/a/artifacts/b" }, false},
		{"pvc-dotdot-segment", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.SubPath = "/a/../b" }, false},
		{"pvc-artifacts-substring", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.SubPath = "/my-artifacts" }, true},
		{"s3-artifacts-segment", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Prefix = "artifacts/x" }, false},
		{"s3-claims-segment", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Prefix = ".claims/x" }, false},
		{"s3-absolute-prefix", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Prefix = "/team-a" }, false},
		{"s3-endpoint-with-path", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Endpoint = "https://s3.example.com/bucket" }, false},
		{"s3-endpoint-not-url", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Endpoint = "s3.example.com" }, false},
		{"s3-bucket-uppercase", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Bucket = "Snapshots" }, false},
		{"s3-bucket-short", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Bucket = "ab" }, false},
		{"s3-http-without-allow", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Endpoint = "http://s3.example.com" }, false},
		{"s3-http-with-allow", s3Store, func(c *snapshotv1.SnapshotStore) {
			c.Spec.S3.Endpoint = "http://s3.example.com"
			c.Spec.S3.AllowHTTP = true
		}, true},
		{"s3-dotdot-segment", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Prefix = "x/../y" }, false},
		{"s3-missing-endpoint", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Endpoint = "" }, false},
		{"s3-missing-region", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Region = "" }, false},
		{"s3-missing-bucket", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Bucket = "" }, false},
		{"s3-path-addressing", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Addressing = "path" }, true},
		{"s3-unknown-addressing", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Addressing = "bucket" }, false},
		{"s3-secret-ref", s3Store, func(c *snapshotv1.SnapshotStore) {
			c.Spec.S3.CredentialsSecretRef = &snapshotv1.SecretKeyReference{Name: "s3-credentials"}
		}, true},
		{"s3-secret-ref-no-name", s3Store, func(c *snapshotv1.SnapshotStore) {
			c.Spec.S3.CredentialsSecretRef = &snapshotv1.SecretKeyReference{Key: "credentials"}
		}, false},
		{"name-63", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Name = strings.Repeat("a", 63) }, true},
		{"name-64", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Name = strings.Repeat("a", 64) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.build("case-" + tc.name)
			if tc.change != nil {
				tc.change(store)
			}
			err := cl.Create(ctx, store)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.True(t, isInvalid(err), "expected an admission error, got %v", err)
			}
		})
	}

	t.Run("defaults", func(t *testing.T) {
		store := s3Store("defaults")
		store.Spec.S3.CredentialsSecretRef = &snapshotv1.SecretKeyReference{Name: "s3-credentials"}
		store.Spec.S3.CABundleConfigMapRef = &snapshotv1.ConfigMapKeyReference{Name: "s3-ca"}
		require.NoError(t, cl.Create(ctx, store))
		stored := &snapshotv1.SnapshotStore{}
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(store), stored))
		assert.Equal(t, snapshotv1.ReclaimPolicyDelete, stored.Spec.ReclaimPolicy)
		assert.Equal(t, "credentials", stored.Spec.S3.CredentialsSecretRef.Key)
		assert.Equal(t, "ca.crt", stored.Spec.S3.CABundleConfigMapRef.Key)

		pvc := pvcStore("defaults-pvc")
		pvc.Spec.PVC.SubPath = ""
		require.NoError(t, cl.Create(ctx, pvc))
		storedPVC := &snapshotv1.SnapshotStore{}
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(pvc), storedPVC))
		assert.Equal(t, storage.PVCRoot, storedPVC.Spec.PVC.SubPath, "subPath defaults to the claim root")
	})
}

func TestSnapshotStoreUpdateAdmission(t *testing.T) {
	cl := startStoreEnvironment(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name   string
		build  func(name string) *snapshotv1.SnapshotStore
		change func(*snapshotv1.SnapshotStore)
		valid  bool
	}{
		{"pvc-claim", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.ClaimName = "other" }, false},
		{"pvc-sub-path", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.SubPath = "/other" }, false},
		{"pvc-sub-path-dropped", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.PVC.SubPath = "" }, false},
		{"pvc-reclaim-policy", pvcStore, func(c *snapshotv1.SnapshotStore) {
			c.Spec.ReclaimPolicy = snapshotv1.ReclaimPolicyRetain
		}, true},
		{"s3-endpoint", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Endpoint = "https://other.example.com" }, false},
		{"s3-region", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Region = "region-2" }, false},
		{"s3-bucket", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Bucket = "other" }, false},
		{"s3-prefix", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Prefix = "team-b" }, false},
		{"s3-prefix-dropped", s3Store, func(c *snapshotv1.SnapshotStore) { c.Spec.S3.Prefix = "" }, false},
		{"s3-reclaim-policy", s3Store, func(c *snapshotv1.SnapshotStore) {
			c.Spec.ReclaimPolicy = snapshotv1.ReclaimPolicyRetain
		}, true},
		{"s3-credentials", s3Store, func(c *snapshotv1.SnapshotStore) {
			c.Spec.S3.CredentialsSecretRef = &snapshotv1.SecretKeyReference{Name: "rotated", Key: "credentials"}
		}, true},
		{"s3-ca-bundle", s3Store, func(c *snapshotv1.SnapshotStore) {
			c.Spec.S3.CABundleConfigMapRef = &snapshotv1.ConfigMapKeyReference{Name: "ca", Key: "ca.crt"}
		}, true},
		{"s3-addressing-and-http", s3Store, func(c *snapshotv1.SnapshotStore) {
			c.Spec.S3.Addressing = "path"
			c.Spec.S3.AllowHTTP = true
		}, true},
		{"pvc-to-s3", pvcStore, func(c *snapshotv1.SnapshotStore) {
			c.Spec.PVC = nil
			c.Spec.S3 = s3Store("x").Spec.S3
		}, false},
		{"s3-to-pvc", s3Store, func(c *snapshotv1.SnapshotStore) {
			c.Spec.S3 = nil
			c.Spec.PVC = pvcStore("x").Spec.PVC
		}, false},
		{"pvc-gains-s3", pvcStore, func(c *snapshotv1.SnapshotStore) { c.Spec.S3 = s3Store("x").Spec.S3 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.build("update-" + tc.name)
			require.NoError(t, cl.Create(ctx, store))
			stored := &snapshotv1.SnapshotStore{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(store), stored))
			tc.change(stored)
			err := cl.Update(ctx, stored)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.True(t, isInvalid(err), "expected an admission error, got %v", err)
			}
		})
	}

	for _, field := range []struct {
		name  string
		block string
		key   string
		seed  func() map[string]any
	}{
		{"s3-prefix", "s3", "prefix", func() map[string]any {
			return map[string]any{"endpoint": "https://s3.example.com", "region": "region-1", "bucket": "snapshots", "prefix": ""}
		}},
	} {
		t.Run("empty-"+field.name+"-dropped", func(t *testing.T) {
			store := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": snapshotv1.GroupVersion.String(),
				"kind":       snapshotv1.KindSnapshotStore,
				"metadata":   map[string]any{"name": "empty-" + field.name},
				"spec":       map[string]any{field.block: field.seed()},
			}}
			require.NoError(t, cl.Create(ctx, store))
			stored := &snapshotv1.SnapshotStore{}
			require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: store.GetName()}, stored))
			require.NoError(t, cl.Update(ctx, stored), "an update that drops the empty field is not a location change")
		})
	}
}

func TestSnapshotStoreStatusAdmission(t *testing.T) {
	cl := startStoreEnvironment(t)
	ctx := t.Context()
	storeID, err := storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: storage.PVCRoot}.StoreID()
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		storeID string
		valid   bool
	}{
		{"valid", storeID, true},
		{"short", "store-v1-short", false},
		{"uppercase", "store-v1-" + strings.Repeat("A", 64), false},
		{"wrong-version", "store-v2-" + strings.Repeat("a", 64), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := pvcStore("status-" + tc.name)
			require.NoError(t, cl.Create(ctx, store))
			stored := &snapshotv1.SnapshotStore{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(store), stored))
			stored.Status.StoreID = tc.storeID
			stored.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "StoreReachable",
				Message: "the store answers", LastTransitionTime: metav1.Now(),
			}}
			err := cl.Status().Update(ctx, stored)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.True(t, isInvalid(err), "expected an admission error, got %v", err)
			}
		})
	}
}

func TestSnapshotStoreNameOnRequestsAndContent(t *testing.T) {
	cl := startStoreEnvironment(t)
	ctx := t.Context()
	storeID, err := storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: storage.PVCRoot}.StoreID()
	require.NoError(t, err)
	longName := strings.Repeat("a", 64)

	for _, tc := range []struct {
		name  string
		store string
		valid bool
	}{
		{"unset", "", true},
		{"named", "gold", true},
		{"too-long", longName, false},
	} {
		t.Run("podsnapshot-"+tc.name, func(t *testing.T) {
			snap := newSettingsSnapshot("snap-"+tc.name, snapshotv1.ContainerSnapshotConfig{Name: "main"})
			snap.Spec.Containers = nil
			snap.Spec.SnapshotStoreName = tc.store
			err := cl.Create(ctx, snap)
			if tc.valid {
				require.NoError(t, err)
				stored := &snapshotv1.PodSnapshot{}
				require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(snap), stored))
				assert.Equal(t, tc.store, stored.Spec.SnapshotStoreName)
				stored.Spec.SnapshotStoreName = "other"
				require.True(t, isInvalid(cl.Update(ctx, stored)), "spec is immutable")
			} else {
				require.True(t, isInvalid(err), "expected an admission error, got %v", err)
			}
		})
		t.Run("snapshotjob-"+tc.name, func(t *testing.T) {
			job := newSettingsJob("job-"+tc.name, snapshotv1.ContainerSnapshotConfig{Name: "main"})
			job.Spec.PodSnapshotTemplate.Containers = nil
			job.Spec.PodSnapshotTemplate.SnapshotStoreName = tc.store
			err := cl.Create(ctx, job)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.True(t, isInvalid(err), "expected an admission error, got %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		binding snapshotv1.CheckpointStorageBinding
		valid   bool
	}{
		{"legacy-store-only", snapshotv1.CheckpointStorageBinding{StoreID: storeID}, true},
		{"store-and-policy", snapshotv1.CheckpointStorageBinding{
			StoreID: storeID, SnapshotStoreName: "gold", ReclaimPolicy: snapshotv1.ReclaimPolicyRetain,
		}, true},
		{"store-without-policy", snapshotv1.CheckpointStorageBinding{StoreID: storeID, SnapshotStoreName: "gold"}, false},
		{"policy-unknown", snapshotv1.CheckpointStorageBinding{
			StoreID: storeID, SnapshotStoreName: "gold", ReclaimPolicy: "Archive",
		}, false},
		{"store-too-long", snapshotv1.CheckpointStorageBinding{
			StoreID: storeID, SnapshotStoreName: longName, ReclaimPolicy: snapshotv1.ReclaimPolicyDelete,
		}, false},
	} {
		t.Run("content-"+tc.name, func(t *testing.T) {
			content := newStorageContent(fmt.Sprintf("bound-%s", tc.name), false, storeID)
			content.Spec.Storage = &tc.binding
			err := cl.Create(ctx, content)
			if tc.valid {
				require.NoError(t, err)
				stored := &snapshotv1.PodSnapshotContent{}
				require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(content), stored))
				assert.Equal(t, tc.binding, *stored.Spec.Storage)
				stored.Spec.Storage.ReclaimPolicy = snapshotv1.ReclaimPolicyDelete
				stored.Spec.Storage.SnapshotStoreName = "changed"
				require.True(t, isInvalid(cl.Update(ctx, stored)), "binding is immutable")
			} else {
				require.True(t, isInvalid(err), "expected an admission error, got %v", err)
			}
		})
	}
}
