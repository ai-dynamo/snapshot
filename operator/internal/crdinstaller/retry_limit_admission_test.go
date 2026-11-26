//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	snapshotv1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

// The ceiling is declared by a marker, so only the API server can say whether it
// is enforced. A request asking for more retries than any failure could clear
// costs the node's staging and the agent on every attempt, not just the asker.
func TestCheckpointRetryLimitAdmission(t *testing.T) {
	var defs []*apiextensionsv1.CustomResourceDefinition
	for _, def := range []string{crds.PodSnapshotCRD(), crds.PodSnapshotContentCRD(), crds.SnapshotJobCRD()} {
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
	ctx := t.Context()
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload"}}))

	for _, tc := range []struct {
		limit int32
		valid bool
	}{
		{0, true},
		{10, true},
		{11, false},
		{100, false},
	} {
		t.Run(fmt.Sprintf("limit-%d", tc.limit), func(t *testing.T) {
			name := fmt.Sprintf("retry-%d", tc.limit)

			snap := newSettingsSnapshot(name, snapshotv1.ContainerSnapshotConfig{Name: "main"})
			snap.Spec.CheckpointRetryLimit = ptr.To(tc.limit)
			assertAdmitted(t, cl.Create(ctx, snap), tc.valid, "PodSnapshot")

			job := newSettingsJob(name, snapshotv1.ContainerSnapshotConfig{Name: "main"})
			job.Spec.PodSnapshotTemplate.CheckpointRetryLimit = ptr.To(tc.limit)
			assertAdmitted(t, cl.Create(ctx, job), tc.valid, "SnapshotJob template")

			// The operator writes content, so this bound guards the object the
			// agent reads rather than anything a user submits.
			content := &snapshotv1.PodSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: snapshotv1.PodSnapshotContentSpec{
					PodSnapshotRef: snapshotv1.PodSnapshotReference{Namespace: "workload", Name: name},
					Source: snapshotv1.PodSnapshotContentSource{
						PodRef:   snapshotv1.PodReference{Name: "source", Containers: []string{"main"}},
						NodeName: "node-a",
					},
					CheckpointRetryLimit: ptr.To(tc.limit),
				},
			}
			assertAdmitted(t, cl.Create(ctx, content), tc.valid, "PodSnapshotContent")
		})
	}
}

func assertAdmitted(t *testing.T, err error, want bool, kind string) {
	t.Helper()
	if want {
		require.NoError(t, err, "%s should accept the limit", kind)
		return
	}
	require.Error(t, err, "%s accepted a limit above the maximum", kind)
	assert.True(t, apierrors.IsInvalid(err), "%s rejected it for the wrong reason: %v", kind, err)
}
