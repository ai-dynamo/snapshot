// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// importedArtifactUID is the UID the artifact was captured under, in another cluster: never the
// importing content's own.
const importedArtifactUID = types.UID("6b1e9f0c-2d4a-4c8e-9f3b-7a5d1e2c4b6a")

func makeImportSnapshot() *snapshotv1alpha1.PodSnapshot {
	snap := makeSnapshotForReconcile()
	snap.Spec.Source = snapshotv1alpha1.PodSnapshotSource{
		Artifact: &snapshotv1alpha1.ArtifactReference{UID: importedArtifactUID, Containers: []string{"main"}},
	}
	return snap
}

func getContent(t *testing.T, r *PodSnapshotReconciler, name string) *snapshotv1alpha1.PodSnapshotContent {
	t.Helper()
	content := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: name}, content))
	return content
}

func getSnapshot(t *testing.T, r *PodSnapshotReconciler, name string) *snapshotv1alpha1.PodSnapshot {
	t.Helper()
	snap := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "inference", Name: name}, snap))
	return snap
}

func TestImport_BindsReadyContentNamingTheArtifact(t *testing.T) {
	snap := makeImportSnapshot()
	// No pod exists at all: an import must never look for one.
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap)

	reconcileSnapshot(t, r, snap.Name)

	content := getContent(t, r, "podsnapshotcontent-snap-uid")
	require.NotNil(t, content.Spec.Source.Artifact)
	assert.Equal(t, importedArtifactUID, content.Spec.Source.Artifact.UID)
	assert.Equal(t, []string{"main"}, content.Spec.Source.Artifact.Containers)
	assert.Nil(t, content.Spec.Source.PodRef)
	assert.Empty(t, content.Spec.Source.NodeName)
	assert.Equal(t, snapshotv1alpha1.PodSnapshotReference{Namespace: "inference", Name: snap.Name, UID: "snap-uid"},
		content.Spec.PodSnapshotRef)
	// No node label, so no agent's capture informer sees it; the artifact UID label is what keeps
	// the artifact out of the orphan sweep.
	assert.Equal(t, map[string]string{snapshotv1alpha1.ArtifactUIDLabel: string(importedArtifactUID)}, content.Labels)
	assert.Empty(t, content.Finalizers, "an import does not own its artifact, so nothing cleans it up")
	ready := meta.FindStatusCondition(content.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonArtifactImported, ready.Reason)
	assert.Equal(t, importedArtifactUID, content.ArtifactUID())

	bound := getSnapshot(t, r, snap.Name)
	assert.Equal(t, "podsnapshotcontent-snap-uid", ptr.Deref(bound.Status.BoundPodSnapshotContentName, ""))

	// The bound pass mirrors the content: Ready, with the import's reason.
	reconcileSnapshot(t, r, snap.Name)
	mirrored := getSnapshot(t, r, snap.Name)
	assert.True(t, snapshotv1alpha1.IsPodSnapshotSucceeded(mirrored))
	assert.False(t, snapshotv1alpha1.IsPodSnapshotFailed(mirrored))
	assert.Equal(t, snapshotv1alpha1.ReasonArtifactImported,
		meta.FindStatusCondition(mirrored.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady).Reason)
}

func TestImport_AdoptsTheContentAnEarlierPassCreated(t *testing.T) {
	snap := makeImportSnapshot()
	// A pass that created the content and died before marking it Ready or binding.
	left := buildImportedPodSnapshotContent(snap, "podsnapshotcontent-snap-uid")
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, left)

	reconcileSnapshot(t, r, snap.Name)

	assert.True(t, snapshotv1alpha1.IsPodSnapshotContentSucceeded(getContent(t, r, "podsnapshotcontent-snap-uid")))
	assert.Equal(t, "podsnapshotcontent-snap-uid",
		ptr.Deref(getSnapshot(t, r, snap.Name).Status.BoundPodSnapshotContentName, ""))
}

func TestImport_AContentOfAnotherPodSnapshotIsAConflict(t *testing.T) {
	snap := makeImportSnapshot()
	theirs := buildImportedPodSnapshotContent(snap, "podsnapshotcontent-snap-uid")
	theirs.Spec.PodSnapshotRef.UID = "another-snapshot"
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, theirs)

	reconcileSnapshot(t, r, snap.Name)

	failed := getSnapshot(t, r, snap.Name)
	assert.True(t, snapshotv1alpha1.IsPodSnapshotFailed(failed))
	assert.Equal(t, "ContentConflict",
		meta.FindStatusCondition(failed.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
	assert.False(t, snapshotv1alpha1.IsPodSnapshotContentSucceeded(getContent(t, r, "podsnapshotcontent-snap-uid")),
		"another PodSnapshot's content is not ours to mark Ready")
}

func TestImport_BoundButNotReadyIsFinishedNotFailed(t *testing.T) {
	// The Ready write was lost after the binding landed. A capture in that state would go looking
	// for its source pod and, finding none, fail; an import has none and must finish instead.
	snap := makeImportSnapshot()
	snap.Status.BoundPodSnapshotContentName = ptr.To("podsnapshotcontent-snap-uid")
	content := buildImportedPodSnapshotContent(snap, "podsnapshotcontent-snap-uid")
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, content)

	reconcileSnapshot(t, r, snap.Name)

	assert.True(t, snapshotv1alpha1.IsPodSnapshotContentSucceeded(getContent(t, r, "podsnapshotcontent-snap-uid")))
	finished := getSnapshot(t, r, snap.Name)
	assert.True(t, snapshotv1alpha1.IsPodSnapshotSucceeded(finished))
	assert.False(t, snapshotv1alpha1.IsPodSnapshotFailed(finished))
}

func TestImport_DeletingThePodSnapshotDeletesItsContent(t *testing.T) {
	snap := makeImportSnapshot()
	snap.Status.BoundPodSnapshotContentName = ptr.To("podsnapshotcontent-snap-uid")
	snap.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	content := buildImportedPodSnapshotContent(snap, "podsnapshotcontent-snap-uid")
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap, content)

	reconcileSnapshot(t, r, snap.Name)

	err := r.Get(context.Background(), types.NamespacedName{Name: "podsnapshotcontent-snap-uid"},
		&snapshotv1alpha1.PodSnapshotContent{})
	assert.True(t, apierrors.IsNotFound(err), "the content goes with its PodSnapshot: %v", err)
	err = r.Get(context.Background(), client.ObjectKeyFromObject(snap), &snapshotv1alpha1.PodSnapshot{})
	assert.True(t, apierrors.IsNotFound(err), "the PodSnapshot finalizer is released: %v", err)
}

func TestImport_IsNotIndexedAsASourcePod(t *testing.T) {
	assert.Empty(t, podSnapshotSourcePodIndexValues(makeImportSnapshot()),
		"pod events must not wake an import")
}

func TestSnapshotReconciler_NoSourceAtAllFails(t *testing.T) {
	snap := makeSnapshotForReconcile()
	snap.Spec.Source = snapshotv1alpha1.PodSnapshotSource{}
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap)

	reconcileSnapshot(t, r, snap.Name)

	failed := getSnapshot(t, r, snap.Name)
	assert.True(t, snapshotv1alpha1.IsPodSnapshotFailed(failed))
	assert.Equal(t, "InvalidSource",
		meta.FindStatusCondition(failed.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
}

func TestImport_CarriesTheContainerSettingsItsRestoresUse(t *testing.T) {
	snap := makeImportSnapshot()
	snap.Spec.Containers = []snapshotv1alpha1.ContainerSnapshotConfig{{
		Name:      "main",
		Lifecycle: &snapshotv1alpha1.SnapshotLifecycle{PostRestore: &snapshotv1alpha1.PostRestoreAction{PeriodSeconds: 3}},
	}}
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap)
	r.EnableLifecycleHooks = true

	reconcileSnapshot(t, r, snap.Name)

	content := getContent(t, r, "podsnapshotcontent-snap-uid")
	assert.Equal(t, snap.Spec.Containers, content.Spec.Containers)
	assert.Nil(t, content.Spec.Storage,
		"an imported artifact was published nowhere here, so it has no store to be bound to")
}

func TestImport_LifecycleHooksAreGatedAsForACheckpoint(t *testing.T) {
	snap := makeImportSnapshot()
	snap.Spec.Containers = []snapshotv1alpha1.ContainerSnapshotConfig{{
		Name:      "main",
		Lifecycle: &snapshotv1alpha1.SnapshotLifecycle{PostRestore: &snapshotv1alpha1.PostRestoreAction{PeriodSeconds: 3}},
	}}
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap)
	r.StoreID = "store-v1-" + strings.Repeat("a", 64)

	reconcileSnapshot(t, r, snap.Name)

	failed := getSnapshot(t, r, snap.Name)
	assert.True(t, snapshotv1alpha1.IsPodSnapshotFailed(failed))
	assert.Equal(t, podSnapshotReasonLifecycleHooksDisabled,
		meta.FindStatusCondition(failed.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
	err := r.Get(context.Background(), types.NamespacedName{Name: "podsnapshotcontent-snap-uid"},
		&snapshotv1alpha1.PodSnapshotContent{})
	assert.True(t, apierrors.IsNotFound(err), "no content for a refused import: %v", err)
}

func TestImport_IsNeverBoundToTheConfiguredStore(t *testing.T) {
	snap := makeImportSnapshot()
	r := makeSnapshotReconciler(snapshotReconcilerScheme(), snap)
	r.StoreID = "store-v1-" + strings.Repeat("a", 64)

	reconcileSnapshot(t, r, snap.Name)

	assert.Nil(t, getContent(t, r, "podsnapshotcontent-snap-uid").Spec.Storage,
		"a binding promises a publication descriptor PageBroker recorded here")
}
