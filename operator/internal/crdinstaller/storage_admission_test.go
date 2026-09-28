//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	snapshotv1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

// Requires envtest's kube-apiserver/etcd binaries via KUBEBUILDER_ASSETS.
// Unlike a fake client, this verifies CEL, map-list uniqueness and /status writes.
func TestContentStorageAdmission(t *testing.T) {
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal([]byte(crds.PodSnapshotContentCRD()), &crd))
	env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{&crd}}
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	cfg, err := env.Start()
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := t.Context()
	const storeID = "store-v1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	for _, bound := range []bool{false, true} {
		name := "legacy"
		if bound {
			name = "bound"
		}
		t.Run(name, func(t *testing.T) {
			content := newStorageContent(name, bound, storeID)
			require.NoError(t, cl.Create(ctx, content))
			stored := &snapshotv1.PodSnapshotContent{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(content), stored))
			assert.Equal(t, content.Spec.Storage, stored.Spec.Storage)
			// Ordinary legacy status updates remain valid without publications.
			stored.Status.Conditions = []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "Captured",
				Message: "capture complete", LastTransitionTime: metav1.Now(),
			}}
			require.NoError(t, cl.Status().Update(ctx, stored))
			changed := stored.DeepCopy()
			changed.Spec.Storage = &snapshotv1.CheckpointStorageBinding{
				StoreID: "store-v1-" + strings.Repeat("b", 64),
			}
			err := cl.Update(ctx, changed)
			require.True(t, apierrors.IsInvalid(err), "adding/changing immutable binding: %v", err)
			if bound {
				removed := stored.DeepCopy()
				removed.Spec.Storage = nil
				err := cl.Update(ctx, removed)
				require.True(t, apierrors.IsInvalid(err), "removing immutable binding: %v", err)
			}
		})
	}

	for i, id := range []string{"", "store-v1-short", "store-v1-" + strings.Repeat("A", 64)} {
		t.Run("invalid-binding-"+string(rune('a'+i)), func(t *testing.T) {
			content := newStorageContent("invalid-binding", true, id)
			err := cl.Create(ctx, content)
			require.True(t, apierrors.IsInvalid(err), "invalid store ID accepted: %v", err)
		})
	}

	for _, tc := range []struct {
		name   string
		bound  bool
		change func(*snapshotv1.CheckpointStorageStatus)
		valid  bool
	}{
		{"confirmed", true, nil, true},
		{"without-binding", false, nil, false},
		{"duplicate-container", true, func(s *snapshotv1.CheckpointStorageStatus) {
			s.Artifacts = append(s.Artifacts, s.Artifacts[0])
		}, false},
		{"uncaptured-container", true, func(s *snapshotv1.CheckpointStorageStatus) {
			s.Artifacts[0].ContainerName = "other"
		}, false},
		{"missing-container", true, func(s *snapshotv1.CheckpointStorageStatus) {
			s.Artifacts[0].ContainerName = ""
		}, false},
		{"empty-list", true, func(s *snapshotv1.CheckpointStorageStatus) {
			s.Artifacts = nil
		}, false},
		{"empty-handle", true, func(s *snapshotv1.CheckpointStorageStatus) {
			s.Artifacts[0].ArtifactHandle = ""
		}, false},
		{"blank-handle", true, func(s *snapshotv1.CheckpointStorageStatus) {
			s.Artifacts[0].ArtifactHandle = " \t"
		}, false},
		{"long-handle", true, func(s *snapshotv1.CheckpointStorageStatus) {
			s.Artifacts[0].ArtifactHandle = strings.Repeat("a", 4097)
		}, false},
		{"empty-format", true, func(s *snapshotv1.CheckpointStorageStatus) {
			s.Artifacts[0].ArtifactFormatVersion = ""
		}, false},
		{"future-format", true, func(s *snapshotv1.CheckpointStorageStatus) {
			s.Artifacts[0].ArtifactFormatVersion = "future.backend/v2"
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifyStorageStatus(t, ctx, cl, tc.name, tc.bound, storeID, tc.change, tc.valid)
		})
	}
}

func newStorageContent(name string, bound bool, storeID string) *snapshotv1.PodSnapshotContent {
	content := &snapshotv1.PodSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: snapshotv1.PodSnapshotContentSpec{
			PodSnapshotRef: snapshotv1.PodSnapshotReference{Namespace: "workload", Name: "snapshot"},
			Source: snapshotv1.PodSnapshotContentSource{
				NodeName: "node-a",
				PodRef:   snapshotv1.PodReference{Name: "source", Containers: []string{"main"}},
			},
		},
	}
	if bound {
		content.Spec.Storage = &snapshotv1.CheckpointStorageBinding{StoreID: storeID}
	}
	return content
}

func verifyStorageStatus(t *testing.T, ctx context.Context, cl client.Client, name string, bound bool,
	storeID string, change func(*snapshotv1.CheckpointStorageStatus), valid bool,
) {
	t.Helper()
	content := newStorageContent(name, bound, storeID)
	require.NoError(t, cl.Create(ctx, content))
	content.Status.Storage = &snapshotv1.CheckpointStorageStatus{Artifacts: []snapshotv1.PublishedContainerArtifact{{
		ContainerName: "main", ArtifactHandle: "opaque-handle", ArtifactFormatVersion: "snapshot.pagebroker/v1",
	}}}
	if change != nil {
		change(content.Status.Storage)
	}
	err := cl.Status().Update(ctx, content)
	if !valid {
		require.True(t, apierrors.IsInvalid(err), "invalid publication accepted: %v", err)
		return
	}
	require.NoError(t, err)
	stored := &snapshotv1.PodSnapshotContent{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(content), stored))
	assert.Equal(t, content.Status.Storage, stored.Status.Storage)
}
