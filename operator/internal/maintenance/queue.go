// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/maintenance/backends"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// maxKeyRetries bounds retries before a failing work item is dropped; the
// next sweep or reconcile picks it back up rather than retrying forever.
const maxKeyRetries = 15

// Queue runs PodSnapshotContent artifact cleanup as a bounded pool of
// worker goroutines draining a rate-limiting workqueue. It implements
// manager.Runnable and defaults to leader-elected.
type Queue struct {
	client    client.Client
	apiReader client.Reader
	recorder  record.EventRecorder
	config    operatortypes.ArtifactCleanupConfig

	registry          BackendRegistry
	configuredBackend string

	queue workqueue.TypedRateLimitingInterface[WorkItemKey]
}

// NewQueue constructs a Queue; register it with the manager (mgr.Add) to run
// it. It fails if the configured backend has no registered implementation,
// so an unsupported --artifact-cleanup-backend-type cannot reach readiness.
func NewQueue(kubeClient client.Client, apiReader client.Reader, recorder record.EventRecorder, cfg operatortypes.ArtifactCleanupConfig) (*Queue, error) {
	configuredBackend := cfg.BackendType
	if configuredBackend == "" {
		configuredBackend = backends.NamePVC
	}
	q := &Queue{
		client:            kubeClient,
		apiReader:         apiReader,
		recorder:          recorder,
		config:            cfg,
		configuredBackend: configuredBackend,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[WorkItemKey](),
			workqueue.TypedRateLimitingQueueConfig[WorkItemKey]{Name: "podsnapshotcontent-maintenance"},
		),
	}
	q.registry.Init(cfg)
	if _, err := q.backend(); err != nil {
		return nil, err
	}
	return q, nil
}

// backend returns the installation's one configured Backend.
func (q *Queue) backend() (Backend, error) {
	backend, ok := q.registry.Get(q.configuredBackend)
	if !ok {
		return nil, fmt.Errorf("no maintenance backend implementation registered for configured store %q", q.configuredBackend)
	}
	return backend, nil
}

// EnqueueDeleteContent schedules cleanup for one content. Repeated calls for
// the same key coalesce while it is queued or being processed.
func (q *Queue) EnqueueDeleteContent(namespace, name string, uid types.UID) {
	q.queue.Add(newDeleteContentKey(namespace, name, uid))
}

// EnqueueSweep schedules an orphan sweep; repeated calls coalesce.
func (q *Queue) EnqueueSweep() {
	q.queue.Add(newSweepKey())
}

// Start implements manager.Runnable: an immediate sweep, then one per
// config.ScanInterval tick, and a drain on shutdown before returning.
func (q *Queue) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("podsnapshotcontent-maintenance")

	workers := q.config.Workers
	if workers <= 0 {
		workers = 1
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.runWorker(ctx, logger)
		}()
	}

	q.EnqueueSweep()
	ticker := time.NewTicker(q.config.ScanInterval)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-ticker.C:
			q.EnqueueSweep()
		}
	}

	q.queue.ShutDownWithDrain()
	wg.Wait()
	return nil
}

func (q *Queue) runWorker(ctx context.Context, logger logr.Logger) {
	for q.processNextItem(ctx, logger) {
	}
}

// processNextItem reports whether the worker should keep calling it (false
// once the queue is shut down and drained).
func (q *Queue) processNextItem(ctx context.Context, logger logr.Logger) bool {
	key, shutdown := q.queue.Get()
	if shutdown {
		return false
	}
	defer q.queue.Done(key)

	if err := q.process(ctx, key, logger); err != nil {
		if q.queue.NumRequeues(key) < maxKeyRetries {
			logger.Error(err, "Maintenance work item failed; requeuing with backoff",
				"mode", key.Mode, "namespace", key.Namespace, "name", key.Name, "attempt", q.queue.NumRequeues(key)+1)
			q.queue.AddRateLimited(key)
			return true
		}
		logger.Error(err, "Maintenance work item exhausted retries; dropping",
			"mode", key.Mode, "namespace", key.Namespace, "name", key.Name)
	}
	q.queue.Forget(key)
	return true
}

func (q *Queue) process(ctx context.Context, key WorkItemKey, logger logr.Logger) error {
	switch key.Mode {
	case ModeDeleteContent:
		return q.processDeleteContent(ctx, key)
	case ModeSweep:
		return q.processSweep(ctx, logger)
	default:
		return fmt.Errorf("unknown maintenance mode %q", key.Mode)
	}
}
