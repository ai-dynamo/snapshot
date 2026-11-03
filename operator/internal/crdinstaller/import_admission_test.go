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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/ai-dynamo/snapshot/api/storage"
	snapshotv1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

const importedUID = "6b1e9f0c-2d4a-4c8e-9f3b-7a5d1e2c4b6a"

func importArtifact() *snapshotv1.ArtifactReference {
	return &snapshotv1.ArtifactReference{UID: importedUID, Containers: []string{"main"}}
}

func importPodRef() *snapshotv1.PodReference {
	return &snapshotv1.PodReference{Name: "source", Containers: []string{"main"}}
}

// Requires envtest's kube-apiserver/etcd binaries via KUBEBUILDER_ASSETS. The source of both
// kinds is a one-of that only the apiserver's CEL evaluation enforces.
func TestImportSourceAdmission(t *testing.T) {
	env := &envtest.Environment{}
	for _, text := range []string{crds.PodSnapshotCRD(), crds.PodSnapshotContentCRD()} {
		var crd apiextensionsv1.CustomResourceDefinition
		require.NoError(t, yaml.Unmarshal([]byte(text), &crd))
		env.CRDs = append(env.CRDs, &crd)
	}
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

	for i, tc := range []struct {
		name   string
		source snapshotv1.PodSnapshotSource
		valid  bool
	}{
		{"pod", snapshotv1.PodSnapshotSource{PodRef: importPodRef()}, true},
		{"artifact", snapshotv1.PodSnapshotSource{Artifact: importArtifact()}, true},
		{"both", snapshotv1.PodSnapshotSource{PodRef: importPodRef(), Artifact: importArtifact()}, false},
		{"neither", snapshotv1.PodSnapshotSource{}, false},
		{"uid-not-a-uuid", snapshotv1.PodSnapshotSource{Artifact: &snapshotv1.ArtifactReference{
			UID: "../escape", Containers: []string{"main"}}}, false},
		{"uid-uppercase", snapshotv1.PodSnapshotSource{Artifact: &snapshotv1.ArtifactReference{
			UID: "6B1E9F0C-2D4A-4C8E-9F3B-7A5D1E2C4B6A", Containers: []string{"main"}}}, false},
		{"two-containers", snapshotv1.PodSnapshotSource{Artifact: &snapshotv1.ArtifactReference{
			UID: importedUID, Containers: []string{"main", "side"}}}, false},
	} {
		t.Run("podsnapshot-"+tc.name, func(t *testing.T) {
			snap := &snapshotv1.PodSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("snap-%d", i), Namespace: "workload"},
				Spec:       snapshotv1.PodSnapshotSpec{Source: tc.source},
			}
			err := cl.Create(ctx, snap)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.True(t, apierrors.IsInvalid(err), "accepted: %v", err)
		})
	}

	for i, tc := range []struct {
		name   string
		source snapshotv1.PodSnapshotContentSource
		valid  bool
	}{
		{"capture", snapshotv1.PodSnapshotContentSource{PodRef: importPodRef(), NodeName: "node-a"}, true},
		{"import", snapshotv1.PodSnapshotContentSource{Artifact: importArtifact()}, true},
		{"import-with-pod", snapshotv1.PodSnapshotContentSource{Artifact: importArtifact(), PodRef: importPodRef()}, false},
		{"import-with-node", snapshotv1.PodSnapshotContentSource{Artifact: importArtifact(), NodeName: "node-a"}, false},
		{"capture-without-node", snapshotv1.PodSnapshotContentSource{PodRef: importPodRef()}, false},
		{"neither", snapshotv1.PodSnapshotContentSource{}, false},
	} {
		t.Run("content-"+tc.name, func(t *testing.T) {
			content := &snapshotv1.PodSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("content-%d", i)},
				Spec: snapshotv1.PodSnapshotContentSpec{
					PodSnapshotRef: snapshotv1.PodSnapshotReference{Namespace: "workload", Name: "snapshot"},
					Source:         tc.source,
				},
			}
			err := cl.Create(ctx, content)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.True(t, apierrors.IsInvalid(err), "accepted: %v", err)
		})
	}

	// Container settings name the checkpointed containers; for an import those are the artifact's.
	for i, tc := range []struct {
		name      string
		container string
		valid     bool
	}{{"settings-for-the-artifacts-container", "main", true}, {"settings-for-another-container", "side", false}} {
		t.Run("podsnapshot-"+tc.name, func(t *testing.T) {
			snap := &snapshotv1.PodSnapshot{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("snap-settings-%d", i), Namespace: "workload"},
				Spec: snapshotv1.PodSnapshotSpec{
					Source:     snapshotv1.PodSnapshotSource{Artifact: importArtifact()},
					Containers: []snapshotv1.ContainerSnapshotConfig{{Name: tc.container}},
				},
			}
			err := cl.Create(ctx, snap)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.True(t, apierrors.IsInvalid(err), "accepted: %v", err)
		})
		t.Run("content-"+tc.name, func(t *testing.T) {
			content := &snapshotv1.PodSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("content-settings-%d", i)},
				Spec: snapshotv1.PodSnapshotContentSpec{
					PodSnapshotRef: snapshotv1.PodSnapshotReference{Namespace: "workload", Name: "snapshot"},
					Source:         snapshotv1.PodSnapshotContentSource{Artifact: importArtifact()},
					Containers:     []snapshotv1.ContainerSnapshotConfig{{Name: tc.container}},
				},
			}
			err := cl.Create(ctx, content)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.True(t, apierrors.IsInvalid(err), "accepted: %v", err)
		})
	}

	// The publication rule names the checkpointed containers; for an import those are the artifact's.
	storeID, err := storage.PVC{Namespace: "snapshot", ClaimName: "snapshot-pvc", BasePath: storage.PVCRoot}.StoreID()
	require.NoError(t, err)
	for _, tc := range []struct {
		container string
		valid     bool
	}{{"main", true}, {"other", false}} {
		t.Run("import-publication-"+tc.container, func(t *testing.T) {
			content := &snapshotv1.PodSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{Name: "import-publication-" + tc.container},
				Spec: snapshotv1.PodSnapshotContentSpec{
					PodSnapshotRef: snapshotv1.PodSnapshotReference{Namespace: "workload", Name: "snapshot"},
					Source:         snapshotv1.PodSnapshotContentSource{Artifact: importArtifact()},
					Storage:        &snapshotv1.CheckpointStorageBinding{StoreID: storeID},
				},
			}
			require.NoError(t, cl.Create(ctx, content))
			content.Status.Storage = &snapshotv1.CheckpointStorageStatus{Artifacts: []snapshotv1.PublishedContainerArtifact{{
				ContainerName: tc.container, ArtifactHandle: "artifacts/" + importedUID + "/containers/" + tc.container,
			}}}
			err := cl.Status().Update(ctx, content)
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.True(t, apierrors.IsInvalid(err), "accepted: %v", err)
		})
	}
}
