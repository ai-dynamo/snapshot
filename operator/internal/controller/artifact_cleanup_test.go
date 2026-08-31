// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/agent/pkg/artifact"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func artifactTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1alpha1.AddToScheme(scheme))
	return scheme
}

func prepareTestArtifactRoot(t *testing.T, uid string) (string, string) {
	t.Helper()
	base := t.TempDir()
	root, err := artifact.ResolveContentRoot(base, uid)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".tmp"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".tmp", "partial"), []byte("x"), 0o600))
	return base, root
}

func TestSnapshotContentReconcilerAddsFinalizer(t *testing.T) {
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-1"), ResourceVersion: "1", Finalizers: []string{"example.com/other"},
	}}
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(artifactTestScheme(t)).WithObjects(content).Build()
	_, err := reconcileSnapshotContent(context.Background(), kubeClient, t.TempDir(), ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.NoError(t, err)

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.ElementsMatch(t, []string{"example.com/other", PodSnapshotContentArtifactCleanupFinalizer}, current.Finalizers)
}

func TestSnapshotContentReconcilerDeletesCompleteRootBeforeFinalizer(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "uid-2")
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-2"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{"example.com/other", PodSnapshotContentArtifactCleanupFinalizer},
	}}
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(artifactTestScheme(t)).WithObjects(content).Build()
	_, err := reconcileSnapshotContent(context.Background(), kubeClient, base, ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.NoError(t, err)
	_, err = os.Lstat(root)
	require.True(t, os.IsNotExist(err))

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.Equal(t, []string{"example.com/other"}, current.Finalizers)
}

func TestSnapshotContentReconcilerRemovesFinalizerWhenArtifactsRootIsAbsent(t *testing.T) {
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-absent"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{PodSnapshotContentArtifactCleanupFinalizer},
	}}
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(artifactTestScheme(t)).WithObjects(content).Build()
	_, err := reconcileSnapshotContent(context.Background(), kubeClient, t.TempDir(), ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.NoError(t, err)

	current := &snapshotv1alpha1.PodSnapshotContent{}
	err = kubeClient.Get(context.Background(), client.ObjectKey{Name: content.Name}, current)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestSnapshotContentReconcilerRetainsFinalizerWhenRootIsUnsafe(t *testing.T) {
	base := t.TempDir()
	root, err := artifact.ResolveContentRoot(base, "uid-3")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(root), 0o750))
	require.NoError(t, os.Symlink(t.TempDir(), root))
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-3"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{PodSnapshotContentArtifactCleanupFinalizer},
	}}
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(artifactTestScheme(t)).WithObjects(content).Build()
	_, err = reconcileSnapshotContent(context.Background(), kubeClient, base, ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.ErrorContains(t, err, "must be a non-symlink directory")

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, kubeClient.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.Contains(t, current.Finalizers, PodSnapshotContentArtifactCleanupFinalizer)
}

type metadataReader struct {
	list  func(*metav1.PartialObjectMetadataList, *client.ListOptions) error
	calls int
}

func (r *metadataReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("unexpected Get")
}

func (r *metadataReader) List(_ context.Context, object client.ObjectList, options ...client.ListOption) error {
	r.calls++
	list, ok := object.(*metav1.PartialObjectMetadataList)
	if !ok {
		return fmt.Errorf("unexpected list type %T", object)
	}
	return r.list(list, (&client.ListOptions{}).ApplyOptions(options))
}

func scannerConfig(base string) operatortypes.ArtifactCleanupConfig {
	return operatortypes.ArtifactCleanupConfig{BasePath: base, ScanInterval: 10 * time.Minute, BatchSize: 10, ListAttempts: 3}
}

func emptyMetadataPage(list *metav1.PartialObjectMetadataList, resourceVersion, continueToken string) {
	list.ResourceVersion = resourceVersion
	list.Continue = continueToken
	list.Items = nil
}

func TestArtifactOrphanScannerDeletesOnFirstAuthoritativeAbsence(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "orphan-uid")
	reader := &metadataReader{list: func(list *metav1.PartialObjectMetadataList, options *client.ListOptions) error {
		require.Equal(t, int64(500), options.Raw.Limit)
		require.Empty(t, options.Raw.ResourceVersion)
		emptyMetadataPage(list, "10", "")
		return nil
	}}
	scanner := &artifactOrphanScanner{apiReader: reader, config: scannerConfig(base)}
	require.NoError(t, scanner.scanOnce(context.Background(), logr.Discard()))
	_, err := os.Lstat(root)
	require.True(t, os.IsNotExist(err))
}

func TestArtifactOrphanScannerProtectsUIDOnFinalPage(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "protected-uid")
	reader := &metadataReader{list: func(list *metav1.PartialObjectMetadataList, options *client.ListOptions) error {
		switch options.Raw.Continue {
		case "":
			emptyMetadataPage(list, "20", "next")
		case "next":
			emptyMetadataPage(list, "20", "")
			list.Items = []metav1.PartialObjectMetadata{{ObjectMeta: metav1.ObjectMeta{Name: "content", UID: types.UID("protected-uid")}}}
		default:
			return fmt.Errorf("unexpected continuation token %q", options.Raw.Continue)
		}
		return nil
	}}
	scanner := &artifactOrphanScanner{apiReader: reader, config: scannerConfig(base)}
	require.NoError(t, scanner.scanOnce(context.Background(), logr.Discard()))
	_, err := os.Lstat(root)
	require.NoError(t, err)
	assert.Equal(t, 2, reader.calls)
}

func TestArtifactOrphanScannerFailsClosedAfterThreeListAttempts(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "orphan-uid")
	reader := &metadataReader{list: func(*metav1.PartialObjectMetadataList, *client.ListOptions) error {
		return errors.New("expired continuation")
	}}
	scanner := &artifactOrphanScanner{apiReader: reader, config: scannerConfig(base)}
	require.Error(t, scanner.scanOnce(context.Background(), logr.Discard()))
	assert.Equal(t, 3, reader.calls)
	_, err := os.Lstat(root)
	require.NoError(t, err)
}

func TestArtifactOrphanScannerProcessesBoundedBatch(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < 11; i++ {
		root, err := artifact.ResolveContentRoot(base, fmt.Sprintf("uid-%02d", i))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(root, 0o750))
	}
	reader := &metadataReader{list: func(list *metav1.PartialObjectMetadataList, _ *client.ListOptions) error {
		emptyMetadataPage(list, "30", "")
		return nil
	}}
	scanner := &artifactOrphanScanner{apiReader: reader, config: scannerConfig(base)}
	require.NoError(t, scanner.scanOnce(context.Background(), logr.Discard()))
	artifactsRoot, err := artifact.ResolveRoot(base)
	require.NoError(t, err)
	entries, err := os.ReadDir(artifactsRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, scanner.scanOnce(context.Background(), logr.Discard()))
	entries, err = os.ReadDir(artifactsRoot)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
