// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
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
	root, err := snapshotv1alpha1.ResolveArtifactRoot(base, uid)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".tmp"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".tmp", "partial"), []byte("x"), 0o600))
	return base, root
}

func TestSnapshotContentReconcilerEnforcesFinalizerMetadataOnly(t *testing.T) {
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-1"), ResourceVersion: "1", Finalizers: []string{"example.com/other"},
	}}
	fakeClient := ctrlfake.NewClientBuilder().WithScheme(artifactTestScheme(t)).WithObjects(content).Build()
	r := &SnapshotContentReconciler{Client: fakeClient, BasePath: t.TempDir()}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.NoError(t, err)

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.ElementsMatch(t, []string{"example.com/other", PodSnapshotContentArtifactCleanupFinalizer}, current.Finalizers)
}

func TestSnapshotContentReconcilerDeletesCompleteRootBeforeFinalizer(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "uid-2")
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-2"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{"example.com/other", PodSnapshotContentArtifactCleanupFinalizer},
	}}
	fakeClient := ctrlfake.NewClientBuilder().WithScheme(artifactTestScheme(t)).WithObjects(content).Build()
	r := &SnapshotContentReconciler{Client: fakeClient, BasePath: base}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.NoError(t, err)
	_, err = os.Lstat(root)
	require.True(t, os.IsNotExist(err))

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.Equal(t, []string{"example.com/other"}, current.Finalizers)
}

func TestSnapshotContentReconcilerRetainsFinalizerOnRemovalFailure(t *testing.T) {
	base, _ := prepareTestArtifactRoot(t, "uid-3")
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-3"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{PodSnapshotContentArtifactCleanupFinalizer},
	}}
	fakeClient := ctrlfake.NewClientBuilder().WithScheme(artifactTestScheme(t)).WithObjects(content).Build()
	r := &SnapshotContentReconciler{
		Client: fakeClient, BasePath: base,
		RemoveAll: func(string) error { return errors.New("PVC unavailable") },
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: content.Name}})
	require.ErrorContains(t, err, "PVC unavailable")

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
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
	parsed := (&client.ListOptions{}).ApplyOptions(options)
	return r.list(list, parsed)
}

func scannerConfig(base string) ArtifactCleanupConfig {
	return ArtifactCleanupConfig{
		BasePath: base, ScanInterval: 10 * time.Minute, OrphanGrace: 5 * time.Minute,
		BatchSize: 10, PageLimit: 500, ListAttempts: 3,
	}
}

func emptyMetadataPage(list *metav1.PartialObjectMetadataList, resourceVersion, continueToken string) {
	list.ResourceVersion = resourceVersion
	list.Continue = continueToken
	list.Items = nil
}

func TestArtifactOrphanScannerAppliesGraceThenDeletes(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "orphan-uid")
	reader := &metadataReader{list: func(list *metav1.PartialObjectMetadataList, options *client.ListOptions) error {
		require.Equal(t, int64(500), options.Raw.Limit)
		require.Empty(t, options.Raw.ResourceVersion)
		emptyMetadataPage(list, "10", "")
		return nil
	}}
	now := time.Unix(100, 0)
	scanner := &ArtifactOrphanScanner{NonCacheReadClient: reader, Config: scannerConfig(base), Now: func() time.Time { return now }}
	require.NoError(t, scanner.ScanOnce(context.Background(), logr.Discard()))
	_, err := os.Lstat(root)
	require.NoError(t, err)

	now = now.Add(5 * time.Minute)
	require.NoError(t, scanner.ScanOnce(context.Background(), logr.Discard()))
	_, err = os.Lstat(root)
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
	now := time.Unix(200, 0)
	scanner := &ArtifactOrphanScanner{NonCacheReadClient: reader, Config: scannerConfig(base), Now: func() time.Time { return now }}
	require.NoError(t, scanner.ScanOnce(context.Background(), logr.Discard()))
	now = now.Add(time.Hour)
	require.NoError(t, scanner.ScanOnce(context.Background(), logr.Discard()))
	_, err := os.Lstat(root)
	require.NoError(t, err)
	assert.Equal(t, 4, reader.calls)
}

func TestArtifactOrphanScannerFailsClosedAfterThreeListAttempts(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "orphan-uid")
	reader := &metadataReader{list: func(*metav1.PartialObjectMetadataList, *client.ListOptions) error {
		return errors.New("expired continuation")
	}}
	scanner := &ArtifactOrphanScanner{NonCacheReadClient: reader, Config: scannerConfig(base)}
	require.Error(t, scanner.ScanOnce(context.Background(), logr.Discard()))
	assert.Equal(t, 3, reader.calls)
	_, err := os.Lstat(root)
	require.NoError(t, err)
}

func TestArtifactOrphanScannerFairBatch(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < 11; i++ {
		root, err := snapshotv1alpha1.ResolveArtifactRoot(base, fmt.Sprintf("uid-%02d", i))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(root, 0o750))
	}
	reader := &metadataReader{list: func(list *metav1.PartialObjectMetadataList, _ *client.ListOptions) error {
		emptyMetadataPage(list, "30", "")
		return nil
	}}
	now := time.Unix(300, 0)
	scanner := &ArtifactOrphanScanner{NonCacheReadClient: reader, Config: scannerConfig(base), Now: func() time.Time { return now }}
	require.NoError(t, scanner.ScanOnce(context.Background(), logr.Discard()))
	now = now.Add(5 * time.Minute)
	require.NoError(t, scanner.ScanOnce(context.Background(), logr.Discard()))
	artifactsRoot, err := snapshotv1alpha1.ResolveArtifactsRoot(base)
	require.NoError(t, err)
	entries, err := os.ReadDir(artifactsRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "uid-10", entries[0].Name())
	require.NoError(t, scanner.ScanOnce(context.Background(), logr.Discard()))
	entries, err = os.ReadDir(artifactsRoot)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestArtifactCleanupConfigDefaultsAndIndependentGrace(t *testing.T) {
	t.Setenv(ArtifactStorageBasePathEnv, "/checkpoints")
	cfg, err := LoadArtifactCleanupConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, cfg.ScanInterval)
	assert.Equal(t, 5*time.Minute, cfg.OrphanGrace)
	assert.Equal(t, 10, cfg.BatchSize)
	assert.Equal(t, int64(500), cfg.PageLimit)
	assert.Equal(t, 3, cfg.ListAttempts)
	assert.NoError(t, cfg.Validate(), "grace may be shorter than scan interval")
}

func TestArtifactCleanupConfigRejectsNonPositiveSettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   string
		value string
	}{
		{name: "zero interval", env: ArtifactScanIntervalEnv, value: "0s"},
		{name: "negative interval", env: ArtifactScanIntervalEnv, value: "-1s"},
		{name: "zero grace", env: ArtifactOrphanGraceEnv, value: "0s"},
		{name: "negative grace", env: ArtifactOrphanGraceEnv, value: "-1s"},
		{name: "zero batch", env: ArtifactBatchSizeEnv, value: "0"},
		{name: "negative batch", env: ArtifactBatchSizeEnv, value: "-1"},
		{name: "zero page limit", env: ArtifactPageLimitEnv, value: "0"},
		{name: "negative page limit", env: ArtifactPageLimitEnv, value: "-1"},
		{name: "zero attempts", env: ArtifactListAttemptsEnv, value: "0"},
		{name: "negative attempts", env: ArtifactListAttemptsEnv, value: "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ArtifactStorageBasePathEnv, "/checkpoints")
			t.Setenv(tc.env, tc.value)
			_, err := LoadArtifactCleanupConfigFromEnv()
			require.Error(t, err)
		})
	}
}

func TestMountInfoContainsPath(t *testing.T) {
	mounted, err := mountInfoContainsPath(
		strings.NewReader("36 25 0:32 / /checkpoints rw,relatime - nfs server:/snapshot rw\n"),
		"/checkpoints",
	)
	require.NoError(t, err)
	assert.True(t, mounted)
}
