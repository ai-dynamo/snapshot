// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"testing"
	"time"

	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/workqueue"
)

func TestEnqueueDeleteContentCoalescesDuplicates(t *testing.T) {
	q, _ := newTestQueue(t, t.TempDir())
	q.queue = workqueue.NewTypedRateLimitingQueue[WorkItemKey](workqueue.DefaultTypedControllerRateLimiter[WorkItemKey]())

	q.EnqueueDeleteContent("ns", "content", "uid-1")
	q.EnqueueDeleteContent("ns", "content", "uid-1")
	q.EnqueueDeleteContent("ns", "content", "uid-1")

	assert.Equal(t, 1, q.queue.Len())
}

func TestEnqueueSweepCoalescesDuplicates(t *testing.T) {
	q, _ := newTestQueue(t, t.TempDir())
	q.queue = workqueue.NewTypedRateLimitingQueue[WorkItemKey](workqueue.DefaultTypedControllerRateLimiter[WorkItemKey]())

	q.EnqueueSweep()
	q.EnqueueSweep()

	assert.Equal(t, 1, q.queue.Len())
}

func TestStartRunsImmediateSweepAndShutsDownCleanly(t *testing.T) {
	q, _ := newTestQueue(t, t.TempDir())
	q.config.Workers = 2
	q.config.ScanInterval = time.Hour
	q.queue = workqueue.NewTypedRateLimitingQueue[WorkItemKey](workqueue.DefaultTypedControllerRateLimiter[WorkItemKey]())

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

func TestNewQueueDefaults(t *testing.T) {
	q := NewQueue(nil, nil, nil, operatortypes.ArtifactCleanupConfig{BasePath: "/checkpoints"})
	require.NotNil(t, q.queue)
	assert.Equal(t, "/checkpoints", q.config.BasePath)
}
