// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/pkg/artifact"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func maintenanceTestScheme(t *testing.T) *runtime.Scheme {
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

func newTestQueue(t *testing.T, basePath string, objects ...client.Object) (*Queue, *record.FakeRecorder) {
	t.Helper()
	kubeClient := ctrlfake.NewClientBuilder().WithScheme(maintenanceTestScheme(t)).WithObjects(objects...).Build()
	recorder := record.NewFakeRecorder(10)
	return &Queue{
		client:    kubeClient,
		apiReader: kubeClient,
		recorder:  recorder,
		config:    operatortypes.ArtifactCleanupConfig{BasePath: basePath, ScanInterval: 0, BatchSize: 10, ListAttempts: 3, Workers: 1},
	}, recorder
}

func TestProcessDeleteContentRemovesRootAndFinalizer(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "uid-2")
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-2"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{"example.com/other", PodSnapshotContentArtifactCleanupFinalizer},
	}}
	q, _ := newTestQueue(t, base, content)

	require.NoError(t, q.processDeleteContent(context.Background(), newDeleteContentKey("", "content", "uid-2")))
	_, err := os.Lstat(root)
	require.True(t, os.IsNotExist(err))

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, q.client.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.Equal(t, []string{"example.com/other"}, current.Finalizers)
}

func TestProcessDeleteContentNoopWhenArtifactsRootAbsent(t *testing.T) {
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-absent"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{PodSnapshotContentArtifactCleanupFinalizer},
	}}
	q, _ := newTestQueue(t, t.TempDir(), content)

	require.NoError(t, q.processDeleteContent(context.Background(), newDeleteContentKey("", "content", "uid-absent")))

	current := &snapshotv1alpha1.PodSnapshotContent{}
	err := q.client.Get(context.Background(), client.ObjectKey{Name: content.Name}, current)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestProcessDeleteContentRetainsFinalizerWhenRootIsUnsafe(t *testing.T) {
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
	q, recorder := newTestQueue(t, base, content)

	err = q.processDeleteContent(context.Background(), newDeleteContentKey("", "content", "uid-3"))
	require.ErrorContains(t, err, "must be a non-symlink directory")

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, q.client.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.Contains(t, current.Finalizers, PodSnapshotContentArtifactCleanupFinalizer)
	assert.Contains(t, <-recorder.Events, "Warning ArtifactCleanupBlocked")
}

func TestProcessDeleteContentRetainsFinalizerWhenArtifactsRootIsSymlink(t *testing.T) {
	base := t.TempDir()
	artifactsRoot, err := artifact.ResolveRoot(base)
	require.NoError(t, err)
	externalRoot := t.TempDir()
	require.NoError(t, os.Symlink(externalRoot, artifactsRoot))
	externalContentRoot := filepath.Join(externalRoot, "uid-4")
	require.NoError(t, os.MkdirAll(externalContentRoot, 0o750))
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("uid-4"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{PodSnapshotContentArtifactCleanupFinalizer},
	}}
	q, recorder := newTestQueue(t, base, content)

	err = q.processDeleteContent(context.Background(), newDeleteContentKey("", "content", "uid-4"))
	require.ErrorContains(t, err, "must be a non-symlink directory")
	_, err = os.Lstat(externalContentRoot)
	require.NoError(t, err, "cleanup must not follow the artifacts symlink")

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, q.client.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.Contains(t, current.Finalizers, PodSnapshotContentArtifactCleanupFinalizer)
	assert.Contains(t, <-recorder.Events, "Warning ArtifactCleanupBlocked")
}

func TestProcessDeleteContentNoopWhenContentGone(t *testing.T) {
	q, _ := newTestQueue(t, t.TempDir())
	require.NoError(t, q.processDeleteContent(context.Background(), newDeleteContentKey("", "missing", "uid-5")))
}

func TestProcessDeleteContentNoopWhenUIDMismatch(t *testing.T) {
	now := metav1.Now()
	content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
		Name: "content", UID: types.UID("current-uid"), ResourceVersion: "1", DeletionTimestamp: &now,
		Finalizers: []string{PodSnapshotContentArtifactCleanupFinalizer},
	}}
	q, _ := newTestQueue(t, t.TempDir(), content)

	require.NoError(t, q.processDeleteContent(context.Background(), newDeleteContentKey("", "content", "stale-uid")))

	current := &snapshotv1alpha1.PodSnapshotContent{}
	require.NoError(t, q.client.Get(context.Background(), client.ObjectKey{Name: content.Name}, current))
	assert.Contains(t, current.Finalizers, PodSnapshotContentArtifactCleanupFinalizer)
}

type metadataReader struct {
	list  func(*metav1.PartialObjectMetadataList, *client.ListOptions) error
	calls int
}

func (r *metadataReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return assert.AnError
}

func (r *metadataReader) List(_ context.Context, object client.ObjectList, options ...client.ListOption) error {
	r.calls++
	list, ok := object.(*metav1.PartialObjectMetadataList)
	if !ok {
		return assert.AnError
	}
	return r.list(list, (&client.ListOptions{}).ApplyOptions(options))
}

func emptyMetadataPage(list *metav1.PartialObjectMetadataList, resourceVersion, continueToken string) {
	list.ResourceVersion = resourceVersion
	list.Continue = continueToken
	list.Items = nil
}

func TestProcessSweepDeletesOnFirstAuthoritativeAbsence(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "orphan-uid")
	reader := &metadataReader{list: func(list *metav1.PartialObjectMetadataList, options *client.ListOptions) error {
		require.Equal(t, int64(500), options.Limit)
		require.Empty(t, options.Continue)
		require.Empty(t, options.Raw.ResourceVersion)
		emptyMetadataPage(list, "10", "")
		return nil
	}}
	q := &Queue{apiReader: reader, config: operatortypes.ArtifactCleanupConfig{BasePath: base, BatchSize: 10, ListAttempts: 3}}
	require.NoError(t, q.processSweep(context.Background(), log.Log))
	_, err := os.Lstat(root)
	require.True(t, os.IsNotExist(err))
}

func TestProcessSweepProtectsUIDOnFinalPage(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "protected-uid")
	reader := &metadataReader{list: func(list *metav1.PartialObjectMetadataList, options *client.ListOptions) error {
		switch options.Continue {
		case "":
			emptyMetadataPage(list, "20", "next")
		case "next":
			emptyMetadataPage(list, "20", "")
			list.Items = []metav1.PartialObjectMetadata{{ObjectMeta: metav1.ObjectMeta{Name: "content", UID: types.UID("protected-uid")}}}
		default:
			return assert.AnError
		}
		return nil
	}}
	q := &Queue{apiReader: reader, config: operatortypes.ArtifactCleanupConfig{BasePath: base, BatchSize: 10, ListAttempts: 3}}
	require.NoError(t, q.processSweep(context.Background(), log.Log))
	_, err := os.Lstat(root)
	require.NoError(t, err)
	assert.Equal(t, 2, reader.calls)
}

func TestProcessSweepFailsClosedAfterListAttemptsExhausted(t *testing.T) {
	base, root := prepareTestArtifactRoot(t, "orphan-uid")
	reader := &metadataReader{list: func(*metav1.PartialObjectMetadataList, *client.ListOptions) error {
		return assert.AnError
	}}
	q := &Queue{apiReader: reader, config: operatortypes.ArtifactCleanupConfig{BasePath: base, BatchSize: 10, ListAttempts: 3}}
	require.Error(t, q.processSweep(context.Background(), log.Log))
	assert.Equal(t, 3, reader.calls)
	_, err := os.Lstat(root)
	require.NoError(t, err)
}

func TestProcessSweepProcessesBoundedBatch(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < 11; i++ {
		root, err := artifact.ResolveContentRoot(base, fmtUID(i))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(root, 0o750))
	}
	reader := &metadataReader{list: func(list *metav1.PartialObjectMetadataList, _ *client.ListOptions) error {
		emptyMetadataPage(list, "30", "")
		return nil
	}}
	q := &Queue{apiReader: reader, config: operatortypes.ArtifactCleanupConfig{BasePath: base, BatchSize: 10, ListAttempts: 3}}
	require.NoError(t, q.processSweep(context.Background(), log.Log))
	artifactsRoot, err := artifact.ResolveRoot(base)
	require.NoError(t, err)
	entries, err := os.ReadDir(artifactsRoot)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, q.processSweep(context.Background(), log.Log))
	entries, err = os.ReadDir(artifactsRoot)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func fmtUID(i int) string {
	const hex = "0123456789"
	return "uid-" + string(hex[i/10]) + string(hex[i%10])
}
