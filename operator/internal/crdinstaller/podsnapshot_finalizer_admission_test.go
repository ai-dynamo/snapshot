//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	snapshotv1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
	"github.com/ai-dynamo/snapshot/operator/internal/controller"
)

// TestPodSnapshotWithEmptyOptionalFieldsReconciles runs the PodSnapshot
// reconciler against a real API server, which is what enforces that the spec is
// immutable. An optional field written as an empty value is stored as written,
// but the Go types leave it out, so the reconciler must never send the spec
// back: the API server would read the missing field as a spec change.
func TestPodSnapshotWithEmptyOptionalFieldsReconciles(t *testing.T) {
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal([]byte(crds.PodSnapshotCRD()), &crd))
	env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{&crd}}
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	cfg, err := env.Start()
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := t.Context()
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload"}}))
	reconciler := &controller.PodSnapshotReconciler{Client: cl, NonCacheReadClient: cl, Recorder: record.NewFakeRecorder(10)}

	for _, tc := range []struct {
		name string
		spec string
	}{
		{"omitted", `{source: {podRef: {name: worker-0, containers: [main]}}}`},
		{"empty-uid", `{source: {podRef: {name: worker-0, uid: "", containers: [main]}}}`},
		{"empty-containers", `{source: {podRef: {name: worker-0, containers: [main]}}, containers: []}`},
		{"empty-http-path", `{source: {podRef: {name: worker-0, containers: [main]}}, ` +
			`containers: [{name: main, checkpointReadinessProbe: {httpGet: {path: "", port: 8000}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unstructured, so the empty value reaches the API server as a user's manifest would send it.
			obj := &unstructured.Unstructured{}
			require.NoError(t, yaml.Unmarshal([]byte(
				"apiVersion: nvidia.com/v1alpha1\nkind: PodSnapshot\nmetadata: {namespace: workload, name: "+tc.name+"}\nspec: "+tc.spec+"\n",
			), &obj.Object))
			require.NoError(t, cl.Create(ctx, obj))
			key := client.ObjectKeyFromObject(obj)
			request := ctrl.Request{NamespacedName: key}

			_, err := reconciler.Reconcile(ctx, request)
			require.NoError(t, err, "adding the finalizer must not be refused as a spec change")
			snap := &snapshotv1.PodSnapshot{}
			require.NoError(t, cl.Get(ctx, key, snap))
			require.Contains(t, snap.Finalizers, "nvidia.com/podsnapshotcontent-cleanup")

			// No source pod exists, so the next pass ends the PodSnapshot through its status.
			_, err = reconciler.Reconcile(ctx, request)
			require.NoError(t, err)
			require.NoError(t, cl.Get(ctx, key, snap))
			failed := meta.FindStatusCondition(snap.Status.Conditions, snapshotv1.PodSnapshotConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, "SourcePodNotFound", failed.Reason)

			require.NoError(t, cl.Delete(ctx, snap))
			_, err = reconciler.Reconcile(ctx, request)
			require.NoError(t, err, "dropping the finalizer must not be refused as a spec change")
			err = cl.Get(ctx, key, snap)
			assert.True(t, apierrors.IsNotFound(err), "the PodSnapshot must be gone once its finalizer is dropped: %v", err)
		})
	}
}
