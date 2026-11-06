// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"fmt"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// controllerName names the reconciler in the manager's logs and metrics.
const controllerName = "coordination-session"

// Config configures Setup. Every field but Clock is required.
type Config struct {
	// Server is the host's coordinator, whose sessions Setup keeps in step
	// with the group resources.
	Server *server.Server

	// Node is the name of the node the host runs on. Its coordinator serves
	// the attempts whose rank-0 member runs on it.
	Node string

	// Clock tells the time that attempts' deadlines are measured against. It
	// defaults to the real clock.
	Clock clock.PassiveClock
}

// validate reports a missing field of cfg.
func (cfg Config) validate() error {
	switch {
	case cfg.Server == nil:
		return fmt.Errorf("the coordinator's groups need its server: %w", session.ErrInvalid)
	case cfg.Node == "":
		return fmt.Errorf("the coordinator's groups need the name of the host's node: %w", session.ErrInvalid)
	}
	return nil
}

// Setup registers with mgr the reconciler and the startup that keep
// cfg.Server's sessions in step with the group resources in attempts' cache.
// attempts is what NewAttempts returned for mgr. The startup marks the server
// ready once it has ensured the session of every unfinished attempt cfg.Node
// serves. mgr's scheme must hold package v1alpha1's kinds. The reconciler and
// the startup log through mgr's logger, named "coordination-session".
func Setup(mgr manager.Manager, attempts *Attempts, cfg Config) error {
	if attempts == nil {
		return fmt.Errorf("the coordinator's groups need the attempts NewAttempts returns: %w", session.ErrInvalid)
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	cache := mgr.GetCache()
	r := &reconciler{attempts: attempts, server: cfg.Server, node: cfg.Node, clock: cfg.Clock}
	log := mgr.GetLogger().WithName(controllerName)
	err := builder.TypedControllerManagedBy[request](mgr).
		Named(controllerName).
		WithOptions(controller.TypedOptions[request]{NeedLeaderElection: ptr.To(false)}).
		WithLogConstructor(func(req *request) logr.Logger {
			if req == nil {
				return log
			}
			return log.WithValues("kind", req.kind, "namespace", req.Namespace, "name", req.Name, "attempt", req.uid)
		}).
		WatchesRawSource(source.TypedKind(cache, &v1alpha1.PodGroupSnapshot{},
			handler.TypedEnqueueRequestsFromMapFunc(snapshotRequests(attempts.reader, log)))).
		WatchesRawSource(source.TypedKind(cache, &v1alpha1.PodGroupRestore{},
			enqueue[*v1alpha1.PodGroupRestore](restoreKind))).
		Complete(r)
	if err != nil {
		return fmt.Errorf("set up the %s controller: %w", controllerName, err)
	}
	if err := mgr.Add(&startup{cache: cache, reconciler: r, server: cfg.Server, log: log}); err != nil {
		return fmt.Errorf("add the startup: %w", err)
	}
	return nil
}
