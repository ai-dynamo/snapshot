// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"os"
	"testing"
	"time"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/maintenance/backends"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEnqueueDeleteContentCoalescesDuplicates(t *testing.T) {
	q, _ := newTestQueue(t, t.TempDir())

	q.EnqueueDeleteContent("ns", "content", "uid-1")
	q.EnqueueDeleteContent("ns", "content", "uid-1")
	q.EnqueueDeleteContent("ns", "content", "uid-1")

	assert.Equal(t, 1, q.queue.Len())
}

func TestEnqueueSweepCoalescesDuplicates(t *testing.T) {
	q, _ := newTestQueue(t, t.TempDir())

	q.EnqueueSweep()
	q.EnqueueSweep()

	assert.Equal(t, 1, q.queue.Len())
}

func TestStartRunsImmediateSweepAndShutsDownCleanly(t *testing.T) {
	q, _ := newTestQueue(t, t.TempDir())
	q.config.Workers = 2
	q.config.ScanInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Start(ctx) }()

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancellation and queue drain")
	}
}

func newQueue(t *testing.T, cfg operatortypes.ArtifactCleanupConfig) *Queue {
	t.Helper()
	q, err := NewQueue(context.Background(), nil, nil, nil, cfg)
	require.NoError(t, err)
	t.Cleanup(q.queue.ShutDown)
	return q
}

func TestNewQueueDefaults(t *testing.T) {
	q := newQueue(t, operatortypes.ArtifactCleanupConfig{BasePath: "/checkpoints"})
	require.NotNil(t, q.queue)
	assert.Equal(t, "/checkpoints", q.config.BasePath)
	assert.Equal(t, backends.NamePVC, q.configuredBackend)
	_, ok := q.registry.Get(backends.NamePVC)
	require.True(t, ok)
}

func TestNewQueueFailsWhenConfiguredBackendIsNotRegistered(t *testing.T) {
	_, err := NewQueue(context.Background(), nil, nil, nil, operatortypes.ArtifactCleanupConfig{BasePath: "/checkpoints", BackendType: "gcs"})
	require.ErrorContains(t, err, `no maintenance backend implementation registered for configured store "gcs"`)
}

func TestNewQueueFailsWhenS3IsConfiguredBackendWithoutS3Config(t *testing.T) {
	_, err := NewQueue(context.Background(), nil, nil, nil, operatortypes.ArtifactCleanupConfig{BackendType: "s3"})
	require.ErrorContains(t, err, "s3 maintenance backend configured without an s3 config")
}

func TestQueueBackendReturnsTheConfiguredBackend(t *testing.T) {
	q := newQueue(t, operatortypes.ArtifactCleanupConfig{BasePath: "/checkpoints"})

	backend, err := q.backend()
	require.NoError(t, err)
	registered, _ := q.registry.Get(backends.NamePVC)
	assert.Same(t, registered, backend)
}

func TestQueueBackendFailsWhenConfiguredBackendIsNotRegistered(t *testing.T) {
	q := newQueue(t, operatortypes.ArtifactCleanupConfig{BasePath: "/checkpoints"})
	q.configuredBackend = "S3"

	_, err := q.backend()
	require.ErrorContains(t, err, `no maintenance backend implementation registered for configured store "S3"`)
}

func TestNewQueueRegistersS3BackendWhenItIsTheConfiguredBackend(t *testing.T) {
	q := newQueue(t, operatortypes.ArtifactCleanupConfig{
		BackendType: "s3",
		S3: &operatortypes.S3Config{
			Bucket: "checkpoints", Region: "us-east-1", CredentialsPath: t.TempDir() + "/credentials",
		},
	})

	backend, ok := q.registry.Get(backends.NameS3)
	require.True(t, ok)
	assert.Equal(t, backends.NameS3, backend.Name())
	_, ok = q.registry.Get(backends.NamePVC)
	assert.False(t, ok, "PVC must not register when S3 is the configured backend")
}

func TestNewQueueDoesNotRegisterS3BackendWhenPVCIsConfigured(t *testing.T) {
	q := newQueue(t, operatortypes.ArtifactCleanupConfig{
		BasePath: "/checkpoints", BackendType: "pvc",
		S3: &operatortypes.S3Config{
			Bucket: "checkpoints", Region: "us-east-1", CredentialsPath: t.TempDir() + "/credentials",
		},
	})

	_, ok := q.registry.Get(backends.NameS3)
	assert.False(t, ok, "S3 must not register when it is not the configured backend")
}

func TestQueueBackendResolvesHelmsLowercaseBackendType(t *testing.T) {
	q := newQueue(t, operatortypes.ArtifactCleanupConfig{BasePath: "/checkpoints", BackendType: "pvc"})

	backend, err := q.backend()
	require.NoError(t, err)
	registered, _ := q.registry.Get(backends.NamePVC)
	assert.Same(t, registered, backend)
}

type failingPatchClient struct {
	client.Client
	fail bool
}

func (c *failingPatchClient) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	if c.fail {
		return apierrors.NewServiceUnavailable("temporary API write failure")
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

func TestSweepRecoversDeleteContentAfterRetryExhaustion(t *testing.T) {
	for _, failure := range []string{"storage failure", "finalizer patch failure"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			base, root := prepareTestArtifactRoot(t, "pending-uid")
			now := metav1.Now()
			content := &snapshotv1alpha1.PodSnapshotContent{ObjectMeta: metav1.ObjectMeta{
				Name: "pending", UID: "pending-uid", ResourceVersion: "1", DeletionTimestamp: &now,
				Finalizers: []string{PodSnapshotContentArtifactCleanupFinalizer},
			}}
			q, recorder := newTestQueue(t, base, content)
			patchClient := &failingPatchClient{Client: q.client, fail: failure == "finalizer patch failure"}
			q.client = patchClient
			if failure == "storage failure" {
				require.NoError(t, os.RemoveAll(root))
				require.NoError(t, os.WriteFile(root, []byte("not a directory"), 0o600))
			}
			// Keep the real retry budget, but avoid waiting for exponential backoff.
			q.queue.ShutDown()
			q.queue = workqueue.NewTypedRateLimitingQueue[WorkItemKey](workqueue.NewTypedItemExponentialFailureRateLimiter[WorkItemKey](0, 0))
			t.Cleanup(q.queue.ShutDown)
			key := newDeleteContentKey("", content.Name, content.UID)
			q.EnqueueDeleteContent(key.Namespace, key.Name, key.UID)
			for attempt := 0; attempt <= maxKeyRetries; attempt++ {
				require.Equal(t, 1, q.queue.Len())
				require.Equal(t, attempt, q.queue.NumRequeues(key))
				require.True(t, q.processNextItem(ctx, logr.Discard()))
				if failure == "storage failure" {
					<-recorder.Events
				}
			}
			require.Zero(t, q.queue.Len())
			require.Zero(t, q.queue.NumRequeues(key))
			current := &snapshotv1alpha1.PodSnapshotContent{}
			require.NoError(t, q.client.Get(ctx, client.ObjectKey{Name: content.Name}, current))
			require.Contains(t, current.Finalizers, PodSnapshotContentArtifactCleanupFinalizer)

			patchClient.fail = false
			if failure == "storage failure" {
				require.NoError(t, os.Remove(root))
				require.NoError(t, os.Mkdir(root, 0o750))
			} else {
				require.NoDirExists(t, root, "artifacts were removed before the finalizer patch failed")
			}
			q.apiReader = &metadataReader{list: func(list *metav1.PartialObjectMetadataList, _ *client.ListOptions) error {
				emptyMetadataPage(list, "10", "")
				list.Items = []metav1.PartialObjectMetadata{{ObjectMeta: current.ObjectMeta}}
				return nil
			}}
			q.EnqueueSweep()
			require.True(t, q.processNextItem(ctx, logr.Discard()))
			require.Equal(t, 1, q.queue.Len(), "sweep must rediscover the dropped deletion")
			require.True(t, q.processNextItem(ctx, logr.Discard()))
			require.Zero(t, q.queue.Len())
			require.NoDirExists(t, root)
			err := q.client.Get(ctx, client.ObjectKey{Name: content.Name}, current)
			require.True(t, apierrors.IsNotFound(err), "finalizer removal must finish deletion: %v", err)
		})
	}
}
