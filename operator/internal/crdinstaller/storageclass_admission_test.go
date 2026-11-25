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

func startClassEnvironment(t *testing.T) client.Client {
	t.Helper()
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
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	require.NoError(t, cl.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload"}}))
	return cl
}

func pvcClass(name string) *snapshotv1.SnapshotStorageClass {
	return &snapshotv1.SnapshotStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: snapshotv1.SnapshotStorageClassSpec{
			PVC: &snapshotv1.PVCStoreSpec{ClaimName: "snapshot-pvc", BasePath: "/models"},
		},
	}
}

func s3Class(name string) *snapshotv1.SnapshotStorageClass {
	return &snapshotv1.SnapshotStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: snapshotv1.SnapshotStorageClassSpec{
			S3: &snapshotv1.S3StoreSpec{
				Endpoint: "https://s3.example.com", Region: "region-1", Bucket: "snapshots", Prefix: "team-a",
			},
		},
	}
}

func isInvalid(err error) bool { return apierrors.IsInvalid(err) }

func TestSnapshotStorageClassCreateAdmission(t *testing.T) {
	cl := startClassEnvironment(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name   string
		build  func(name string) *snapshotv1.SnapshotStorageClass
		change func(*snapshotv1.SnapshotStorageClass)
		valid  bool
	}{
		{"pvc", pvcClass, nil, true},
		{"s3", s3Class, nil, true},
		{"pvc-root", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.PVC.BasePath = "" }, true},
		{"s3-no-prefix", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Prefix = "" }, true},
		{"no-block", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.PVC = nil }, false},
		{"both-blocks", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3 = s3Class("x").Spec.S3 }, false},
		{"reclaim-retain", pvcClass, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.ReclaimPolicy = snapshotv1.ReclaimPolicyRetain
		}, true},
		{"reclaim-unknown", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.ReclaimPolicy = "Archive" }, false},
		{"empty-claim", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.PVC.ClaimName = "" }, false},
		{"pvc-artifacts-segment", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.PVC.BasePath = "/a/artifacts/b" }, false},
		{"pvc-dotdot-segment", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.PVC.BasePath = "/a/../b" }, false},
		{"pvc-artifacts-substring", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.PVC.BasePath = "/my-artifacts" }, true},
		{"s3-artifacts-segment", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Prefix = "artifacts/x" }, false},
		{"s3-dotdot-segment", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Prefix = "x/../y" }, false},
		{"s3-missing-endpoint", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Endpoint = "" }, false},
		{"s3-missing-region", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Region = "" }, false},
		{"s3-missing-bucket", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Bucket = "" }, false},
		{"s3-path-addressing", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Addressing = "path" }, true},
		{"s3-unknown-addressing", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Addressing = "bucket" }, false},
		{"s3-secret-ref", s3Class, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.S3.CredentialsSecretRef = &snapshotv1.SecretKeyReference{Name: "s3-credentials"}
		}, true},
		{"s3-secret-ref-no-name", s3Class, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.S3.CredentialsSecretRef = &snapshotv1.SecretKeyReference{Key: "credentials"}
		}, false},
		{"name-63", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Name = strings.Repeat("a", 63) }, true},
		{"name-64", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Name = strings.Repeat("a", 64) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			class := tc.build("case-" + tc.name)
			if tc.change != nil {
				tc.change(class)
			}
			err := cl.Create(ctx, class)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.True(t, isInvalid(err), "expected an admission error, got %v", err)
			}
		})
	}

	t.Run("defaults", func(t *testing.T) {
		class := s3Class("defaults")
		class.Spec.S3.CredentialsSecretRef = &snapshotv1.SecretKeyReference{Name: "s3-credentials"}
		class.Spec.S3.CABundleConfigMapRef = &snapshotv1.ConfigMapKeyReference{Name: "s3-ca"}
		require.NoError(t, cl.Create(ctx, class))
		stored := &snapshotv1.SnapshotStorageClass{}
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(class), stored))
		assert.Equal(t, snapshotv1.ReclaimPolicyDelete, stored.Spec.ReclaimPolicy)
		assert.Equal(t, "credentials", stored.Spec.S3.CredentialsSecretRef.Key)
		assert.Equal(t, "ca.crt", stored.Spec.S3.CABundleConfigMapRef.Key)
	})
}

func TestSnapshotStorageClassUpdateAdmission(t *testing.T) {
	cl := startClassEnvironment(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name   string
		build  func(name string) *snapshotv1.SnapshotStorageClass
		change func(*snapshotv1.SnapshotStorageClass)
		valid  bool
	}{
		{"pvc-claim", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.PVC.ClaimName = "other" }, false},
		{"pvc-base-path", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.PVC.BasePath = "/other" }, false},
		{"pvc-base-path-dropped", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.PVC.BasePath = "" }, false},
		{"pvc-reclaim-policy", pvcClass, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.ReclaimPolicy = snapshotv1.ReclaimPolicyRetain
		}, true},
		{"s3-endpoint", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Endpoint = "https://other.example.com" }, false},
		{"s3-region", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Region = "region-2" }, false},
		{"s3-bucket", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Bucket = "other" }, false},
		{"s3-prefix", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Prefix = "team-b" }, false},
		{"s3-prefix-dropped", s3Class, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3.Prefix = "" }, false},
		{"s3-reclaim-policy", s3Class, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.ReclaimPolicy = snapshotv1.ReclaimPolicyRetain
		}, true},
		{"s3-credentials", s3Class, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.S3.CredentialsSecretRef = &snapshotv1.SecretKeyReference{Name: "rotated", Key: "credentials"}
		}, true},
		{"s3-ca-bundle", s3Class, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.S3.CABundleConfigMapRef = &snapshotv1.ConfigMapKeyReference{Name: "ca", Key: "ca.crt"}
		}, true},
		{"s3-addressing-and-http", s3Class, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.S3.Addressing = "path"
			c.Spec.S3.AllowHTTP = true
		}, true},
		{"pvc-to-s3", pvcClass, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.PVC = nil
			c.Spec.S3 = s3Class("x").Spec.S3
		}, false},
		{"s3-to-pvc", s3Class, func(c *snapshotv1.SnapshotStorageClass) {
			c.Spec.S3 = nil
			c.Spec.PVC = pvcClass("x").Spec.PVC
		}, false},
		{"pvc-gains-s3", pvcClass, func(c *snapshotv1.SnapshotStorageClass) { c.Spec.S3 = s3Class("x").Spec.S3 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			class := tc.build("update-" + tc.name)
			require.NoError(t, cl.Create(ctx, class))
			stored := &snapshotv1.SnapshotStorageClass{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(class), stored))
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
		{"pvc-basepath", "pvc", "basePath", func() map[string]any {
			return map[string]any{"claimName": "snapshot-pvc", "basePath": ""}
		}},
		{"s3-prefix", "s3", "prefix", func() map[string]any {
			return map[string]any{"endpoint": "https://s3.example.com", "region": "region-1", "bucket": "snapshots", "prefix": ""}
		}},
	} {
		t.Run("empty-"+field.name+"-dropped", func(t *testing.T) {
			class := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": snapshotv1.GroupVersion.String(),
				"kind":       snapshotv1.KindSnapshotStorageClass,
				"metadata":   map[string]any{"name": "empty-" + field.name},
				"spec":       map[string]any{field.block: field.seed()},
			}}
			require.NoError(t, cl.Create(ctx, class))
			stored := &snapshotv1.SnapshotStorageClass{}
			require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: class.GetName()}, stored))
			require.NoError(t, cl.Update(ctx, stored), "an update that drops the empty field is not a location change")
		})
	}
}

func TestSnapshotStorageClassStatusAdmission(t *testing.T) {
	cl := startClassEnvironment(t)
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
			class := pvcClass("status-" + tc.name)
			require.NoError(t, cl.Create(ctx, class))
			stored := &snapshotv1.SnapshotStorageClass{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(class), stored))
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

func TestStorageClassNameOnRequestsAndContent(t *testing.T) {
	cl := startClassEnvironment(t)
	ctx := t.Context()
	storeID, err := storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: storage.PVCRoot}.StoreID()
	require.NoError(t, err)
	longName := strings.Repeat("a", 64)

	for _, tc := range []struct {
		name  string
		class string
		valid bool
	}{
		{"unset", "", true},
		{"named", "gold", true},
		{"too-long", longName, false},
	} {
		t.Run("podsnapshot-"+tc.name, func(t *testing.T) {
			snap := newSettingsSnapshot("snap-"+tc.name, snapshotv1.ContainerSnapshotConfig{Name: "main"})
			snap.Spec.Containers = nil
			snap.Spec.StorageClassName = tc.class
			err := cl.Create(ctx, snap)
			if tc.valid {
				require.NoError(t, err)
				stored := &snapshotv1.PodSnapshot{}
				require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(snap), stored))
				assert.Equal(t, tc.class, stored.Spec.StorageClassName)
				stored.Spec.StorageClassName = "other"
				require.True(t, isInvalid(cl.Update(ctx, stored)), "spec is immutable")
			} else {
				require.True(t, isInvalid(err), "expected an admission error, got %v", err)
			}
		})
		t.Run("snapshotjob-"+tc.name, func(t *testing.T) {
			job := newSettingsJob("job-"+tc.name, snapshotv1.ContainerSnapshotConfig{Name: "main"})
			job.Spec.PodSnapshotTemplate.Containers = nil
			job.Spec.PodSnapshotTemplate.StorageClassName = tc.class
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
		{"class-and-policy", snapshotv1.CheckpointStorageBinding{
			StoreID: storeID, StorageClassName: "gold", ReclaimPolicy: snapshotv1.ReclaimPolicyRetain,
		}, true},
		{"class-without-policy", snapshotv1.CheckpointStorageBinding{StoreID: storeID, StorageClassName: "gold"}, false},
		{"policy-unknown", snapshotv1.CheckpointStorageBinding{
			StoreID: storeID, StorageClassName: "gold", ReclaimPolicy: "Archive",
		}, false},
		{"class-too-long", snapshotv1.CheckpointStorageBinding{
			StoreID: storeID, StorageClassName: longName, ReclaimPolicy: snapshotv1.ReclaimPolicyDelete,
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
				stored.Spec.Storage.StorageClassName = "changed"
				require.True(t, isInvalid(cl.Update(ctx, stored)), "binding is immutable")
			} else {
				require.True(t, isInvalid(err), "expected an admission error, got %v", err)
			}
		})
	}
}
