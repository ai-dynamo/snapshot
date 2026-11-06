// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"context"
	"fmt"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// The cache indexes the attempts are read by, which NewAttempts registers.
const (
	// uidField indexes both kinds by metadata.uid, which Attempts.get looks
	// up.
	uidField = "metadata.uid"

	// snapshotRefField indexes PodGroupRestores by spec.snapshotRef.uid, which
	// snapshotRequests looks up.
	snapshotRefField = "spec.snapshotRef.uid"
)

// indexUID returns the index values of an object under uidField.
func indexUID(obj client.Object) []string { return []string{string(obj.GetUID())} }

// indexSnapshotRef returns the index values of a PodGroupRestore under
// snapshotRefField.
func indexSnapshotRef(obj client.Object) []string {
	restore, ok := obj.(*v1alpha1.PodGroupRestore)
	if !ok {
		return nil
	}
	return []string{string(restore.Spec.SnapshotRef.UID)}
}

// The kinds of a group resource, which a request names.
const (
	snapshotKind = "PodGroupSnapshot"
	restoreKind  = "PodGroupRestore"
)

// request names one group resource: by kind, namespace and name, for the logs
// and the startup's errors, and by UID, which the reconciler reads its attempt
// by. Once the resource is gone, or another UID has taken its name, the UID
// finds no attempt, and the reconciler removes the session.
type request struct {
	kind string
	types.NamespacedName
	uid types.UID
}

// requestFor returns the request of a group resource of kind.
func requestFor(kind string, obj client.Object) request {
	return request{kind: kind, NamespacedName: client.ObjectKeyFromObject(obj), uid: obj.GetUID()}
}

// Attempts reads the attempts of the group resources in a manager's cache, by
// UID.
type Attempts struct {
	reader client.Reader
}

// NewAttempts registers the cache indexes the attempts are read by and returns
// the attempts in mgr's cache. The host calls it before it starts mgr, and
// before it builds anything that reads the attempts, such as its Authorizer.
func NewAttempts(ctx context.Context, mgr manager.Manager) (*Attempts, error) {
	indexer := mgr.GetFieldIndexer()
	for _, obj := range []client.Object{&v1alpha1.PodGroupSnapshot{}, &v1alpha1.PodGroupRestore{}} {
		if err := indexer.IndexField(ctx, obj, uidField, indexUID); err != nil {
			return nil, fmt.Errorf("index %T by UID: %w", obj, err)
		}
	}
	if err := indexer.IndexField(ctx, &v1alpha1.PodGroupRestore{}, snapshotRefField, indexSnapshotRef); err != nil {
		return nil, fmt.Errorf("index the PodGroupRestores by the PodGroupSnapshot they restore: %w", err)
	}
	return &Attempts{reader: mgr.GetCache()}, nil
}

// MemberNodes returns the node of each member of the attempt uid, by member
// ID; a member whose node is not known yet maps to "". It returns an error
// wrapping session.ErrNotFound when the attempt is not known.
func (as *Attempts) MemberNodes(ctx context.Context, uid types.UID) (map[string]string, error) {
	a, err := as.get(ctx, uid)
	if err != nil {
		return nil, err
	}
	return a.nodes, nil
}

// get returns the attempt of the group resource with uid, of either kind. It
// returns nil and an error wrapping session.ErrNotFound when no resource in the
// cache has that UID, or when it is a restore that has no attempt (see
// restoreAttempt).
func (as *Attempts) get(ctx context.Context, uid types.UID) (*attempt, error) {
	var snapshots v1alpha1.PodGroupSnapshotList
	if err := as.reader.List(ctx, &snapshots, client.MatchingFields{uidField: string(uid)}); err != nil {
		return nil, fmt.Errorf("list the PodGroupSnapshots with UID %s: %w", uid, err)
	}
	if len(snapshots.Items) > 0 {
		a := captureAttempt(&snapshots.Items[0])
		return &a, nil
	}
	var restores v1alpha1.PodGroupRestoreList
	if err := as.reader.List(ctx, &restores, client.MatchingFields{uidField: string(uid)}); err != nil {
		return nil, fmt.Errorf("list the PodGroupRestores with UID %s: %w", uid, err)
	}
	if len(restores.Items) > 0 {
		if a, err := as.restore(ctx, &restores.Items[0]); err != nil || a != nil {
			return a, err
		}
	}
	return nil, fmt.Errorf("attempt %s: %w", uid, session.ErrNotFound)
}

// restore returns the attempt of a PodGroupRestore, reading the
// PodGroupSnapshot it restores, or nil if it has none.
func (as *Attempts) restore(ctx context.Context, restore *v1alpha1.PodGroupRestore) (*attempt, error) {
	snapshot := &v1alpha1.PodGroupSnapshot{}
	key := types.NamespacedName{Namespace: restore.Namespace, Name: restore.Spec.SnapshotRef.Name}
	switch err := as.reader.Get(ctx, key, snapshot); {
	case apierrors.IsNotFound(err):
		snapshot = nil
	case err != nil:
		return nil, fmt.Errorf("get the PodGroupSnapshot %s of PodGroupRestore %s: %w", key, restore.Name, err)
	}
	a, ok := restoreAttempt(restore, snapshot)
	if !ok {
		return nil, nil
	}
	return &a, nil
}
