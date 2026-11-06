// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// reconciler keeps the coordinator's sessions in step with the group
// resources: it reads a resource's attempt by UID, decides, and acts on the
// server.
type reconciler struct {
	attempts *Attempts
	server   *server.Server
	node     string
	clock    clock.PassiveClock

	// mu serializes reconciles, the controller's and the startup's, so the
	// startup never ensures a session after a reconcile that saw its resource
	// gone has removed it.
	mu sync.Mutex
}

// Reconcile implements reconcile.TypedReconciler.
func (r *reconciler) Reconcile(ctx context.Context, req request) (reconcile.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, err := r.attempts.get(ctx, req.uid)
	if errors.Is(err, session.ErrNotFound) {
		// The resource is gone, another UID has taken its name, or it is a
		// restore that has no attempt.
		a, err = nil, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	var h held
	h.ended, h.session = r.server.Ended(req.uid)
	act, again := decide(a, r.node, h, r.clock.Now())
	switch act {
	case ensure:
		err = r.server.Ensure(ctx, a.spec)
	case cancel:
		err = r.server.Cancel(ctx, req.uid)
	case remove:
		err = r.server.Remove(ctx, req.uid)
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("%s the session: %w", act, err)
	}
	return reconcile.Result{RequeueAfter: again}, nil
}

// enqueue maps every event of a group resource of kind to its request.
func enqueue[T client.Object](kind string) handler.TypedEventHandler[T, request] {
	return handler.TypedEnqueueRequestsFromMapFunc(func(_ context.Context, obj T) []request {
		return []request{requestFor(kind, obj)}
	})
}

// snapshotRequests maps every event of a PodGroupSnapshot to its own request
// and to the request of each PodGroupRestore that reads it, by UID. A
// restore's attempt reads its PodGroupSnapshot, and the cache may show the
// restore first, whose reconcile then finds no attempt: the snapshot's event
// reconciles it again. When the restores cannot be listed, the event maps to
// the snapshot's own request alone, and the failure is logged.
func snapshotRequests(reader client.Reader, log logr.Logger) handler.TypedMapFunc[*v1alpha1.PodGroupSnapshot, request] {
	return func(ctx context.Context, snapshot *v1alpha1.PodGroupSnapshot) []request {
		reqs := []request{requestFor(snapshotKind, snapshot)}
		var restores v1alpha1.PodGroupRestoreList
		if err := reader.List(ctx, &restores, client.InNamespace(snapshot.Namespace),
			client.MatchingFields{snapshotRefField: string(snapshot.UID)}); err != nil {
			log.Error(err, "Listing the PodGroupRestores of a PodGroupSnapshot failed; reconciling the snapshot alone",
				"namespace", snapshot.Namespace, "name", snapshot.Name)
			return reqs
		}
		for i := range restores.Items {
			reqs = append(reqs, requestFor(restoreKind, &restores.Items[i]))
		}
		return reqs
	}
}
