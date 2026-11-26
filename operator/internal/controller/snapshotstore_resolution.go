// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	ReasonStoreNotFound    = "StoreNotFound"
	ReasonStoreNotReady    = "StoreNotReady"
	ReasonStoreTerminating = "StoreTerminating"
)

// snapshotStoreResolution is the result of looking up the store of a request. It
// is usable when it has no reason. Its store is nil when content is not bound
// to a store.
type snapshotStoreResolution struct {
	Store   *snapshotv1alpha1.SnapshotStore
	Reason  string
	Message string
}

func (r snapshotStoreResolution) Usable() bool { return r.Reason == "" }

// resolveSnapshotStore returns the store a request names, or the default store.
// While binding is off, only the default store is usable.
func resolveSnapshotStore(ctx context.Context, reader client.Reader, requested, defaultName string,
	bindingEnabled bool,
) (snapshotStoreResolution, error) {
	name := requested
	if name == "" {
		name = defaultName
	}
	if !bindingEnabled {
		if name != defaultName {
			return snapshotStoreResolution{
				Reason:  ReasonStoreNotReady,
				Message: fmt.Sprintf("store %q is not reconciled while new contents are not bound to a store", name),
			}, nil
		}
		return snapshotStoreResolution{}, nil
	}

	store := &snapshotv1alpha1.SnapshotStore{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, store); err != nil {
		if apierrors.IsNotFound(err) {
			return snapshotStoreResolution{
				Reason:  ReasonStoreNotFound,
				Message: fmt.Sprintf("store %q does not exist", name),
			}, nil
		}
		return snapshotStoreResolution{}, fmt.Errorf("get SnapshotStore %q: %w", name, err)
	}
	return storeUsability(store), nil
}

// storeUsability says whether new content can be bound through the store.
func storeUsability(store *snapshotv1alpha1.SnapshotStore) snapshotStoreResolution {
	if !store.DeletionTimestamp.IsZero() {
		return snapshotStoreResolution{
			Store:   store,
			Reason:  ReasonStoreTerminating,
			Message: fmt.Sprintf("store %q is being deleted", store.Name),
		}
	}
	cond := meta.FindStatusCondition(store.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || store.Status.StoreID == "" {
		message := fmt.Sprintf("store %q is not ready", store.Name)
		if cond != nil {
			message = fmt.Sprintf("store %q is not ready: %s: %s", store.Name, cond.Reason, cond.Message)
		}
		return snapshotStoreResolution{Store: store, Reason: ReasonStoreNotReady, Message: message}
	}
	return snapshotStoreResolution{Store: store}
}
