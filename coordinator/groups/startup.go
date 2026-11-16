// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// The wait between the startup's tries to ensure every session: it starts at
// firstRetry and doubles up to maxRetry.
const (
	firstRetry = 100 * time.Millisecond
	maxRetry   = 10 * time.Second
)

// startup ensures the session of every unfinished attempt this node serves,
// once the cache has synced, then marks the coordinator ready, so a member
// that reconnects after a coordinator restart finds its session recreated, and
// learns at once that it was lost. It reconciles every group resource through
// the controller's reconciler, whose lock orders the two, so both decide the
// same way and neither undoes the other.
type startup struct {
	cache      cache.Cache
	reconciler *reconciler
	server     *server.Server
	log        logr.Logger
}

// Start implements manager.Runnable. It returns nil once the coordinator is
// ready, or once ctx is done.
func (s *startup) Start(ctx context.Context) error {
	if !s.cache.WaitForCacheSync(ctx) {
		return nil
	}
	for wait := firstRetry; ; wait = min(2*wait, maxRetry) {
		err := s.reconcileAll(ctx)
		if err == nil {
			break
		}
		s.log.Error(err, "Ensuring the sessions this node serves failed; retrying")
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil
		}
	}
	s.server.MarkReady()
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every node's
// coordinator serves its own sessions.
func (s *startup) NeedLeaderElection() bool { return false }

// reconcileAll reconciles every group resource in the cache once.
func (s *startup) reconcileAll(ctx context.Context) error {
	var snapshots v1alpha1.PodGroupSnapshotList
	if err := s.cache.List(ctx, &snapshots); err != nil {
		return fmt.Errorf("list the PodGroupSnapshots: %w", err)
	}
	var restores v1alpha1.PodGroupRestoreList
	if err := s.cache.List(ctx, &restores); err != nil {
		return fmt.Errorf("list the PodGroupRestores: %w", err)
	}
	reqs := make([]request, 0, len(snapshots.Items)+len(restores.Items))
	for i := range snapshots.Items {
		reqs = append(reqs, requestFor(snapshotKind, &snapshots.Items[i]))
	}
	for i := range restores.Items {
		reqs = append(reqs, requestFor(restoreKind, &restores.Items[i]))
	}
	var errs []error
	for _, req := range reqs {
		if _, err := s.reconciler.Reconcile(ctx, req); err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", req.kind, req.NamespacedName, err))
		}
	}
	return errors.Join(errs...)
}
