// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"context"
	"fmt"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

// startup tries to ensure the session of every unfinished attempt this node
// serves, once the cache has synced, then marks the coordinator ready. A member
// that reconnects after a coordinator restart then finds its session recreated,
// and learns at once that it was lost. Startup reconciles every group resource
// once, through the controller's reconciler. The reconciler's lock orders the
// two, so both decide the same way and neither undoes the other.
//
// A reconcile can fail, for example when a session's control bucket finds no
// room in the coordinator's file store. Startup logs the failure and marks the
// coordinator ready all the same. The controller's watches deliver every group
// resource when it starts, and the controller retries each reconcile that
// fails. That is safe. A member that had joined a session before the restart
// reconnects in one of two ways. If the controller has recreated the session,
// the coordinator fails it with session.ReasonCoordinatorLost as the member
// reconnects. If not, the member learns session.ErrCoordinatorLost itself and
// never writes to the later session, which cannot commit without it. A member
// that had not joined yet waits until the controller creates the session.
type startup struct {
	cache      cache.Cache
	reconciler *reconciler
	server     *server.Server
	log        logr.Logger
}

// Start implements manager.Runnable. It returns nil once the coordinator is
// ready, or once ctx is done. A List of the cache's group resources fails only
// on a programming error, so Start returns that error, which stops the
// manager, and leaves the coordinator unready.
func (s *startup) Start(ctx context.Context) error {
	if !s.cache.WaitForCacheSync(ctx) {
		return nil
	}
	reqs, err := s.requests(ctx)
	if err != nil {
		return err
	}
	s.reconcileAll(ctx, reqs)
	if ctx.Err() != nil {
		return nil
	}
	s.server.MarkReady()
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every node's
// coordinator serves its own sessions.
func (s *startup) NeedLeaderElection() bool { return false }

// reconcileAll reconciles each of reqs once, until ctx is done, and logs each
// failure, which the controller retries.
func (s *startup) reconcileAll(ctx context.Context, reqs []request) {
	for _, req := range reqs {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.reconciler.Reconcile(ctx, req); err != nil {
			req.logger(s.log).Error(err, "Reconciling a group resource failed; the controller retries it")
		}
	}
}

// requests returns the request of every group resource in the cache.
func (s *startup) requests(ctx context.Context) ([]request, error) {
	var snapshots v1alpha1.PodGroupSnapshotList
	if err := s.cache.List(ctx, &snapshots); err != nil {
		return nil, fmt.Errorf("list the PodGroupSnapshots: %w", err)
	}
	var restores v1alpha1.PodGroupRestoreList
	if err := s.cache.List(ctx, &restores); err != nil {
		return nil, fmt.Errorf("list the PodGroupRestores: %w", err)
	}
	reqs := make([]request, 0, len(snapshots.Items)+len(restores.Items))
	for i := range snapshots.Items {
		reqs = append(reqs, requestFor(snapshotKind, &snapshots.Items[i]))
	}
	for i := range restores.Items {
		reqs = append(reqs, requestFor(restoreKind, &restores.Items[i]))
	}
	return reqs, nil
}
