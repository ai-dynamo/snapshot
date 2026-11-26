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
	ReasonStorageClassNotFound    = "StorageClassNotFound"
	ReasonStorageClassNotReady    = "StorageClassNotReady"
	ReasonStorageClassTerminating = "StorageClassTerminating"
)

// storageClassResolution is the result of looking up the class of a request. It
// is usable when it has no reason. Its class is nil when content is not bound
// to a store.
type storageClassResolution struct {
	Class   *snapshotv1alpha1.SnapshotStorageClass
	Reason  string
	Message string
}

func (r storageClassResolution) Usable() bool { return r.Reason == "" }

// resolveStorageClass returns the class a request names, or the default class.
// While binding is off, only the default class is usable.
func resolveStorageClass(ctx context.Context, reader client.Reader, requested, defaultName string,
	bindingEnabled bool,
) (storageClassResolution, error) {
	name := requested
	if name == "" {
		name = defaultName
	}
	if !bindingEnabled {
		if name != defaultName {
			return storageClassResolution{
				Reason:  ReasonStorageClassNotReady,
				Message: fmt.Sprintf("class %q is not reconciled while new contents are not bound to a store", name),
			}, nil
		}
		return storageClassResolution{}, nil
	}

	class := &snapshotv1alpha1.SnapshotStorageClass{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, class); err != nil {
		if apierrors.IsNotFound(err) {
			return storageClassResolution{
				Reason:  ReasonStorageClassNotFound,
				Message: fmt.Sprintf("class %q does not exist", name),
			}, nil
		}
		return storageClassResolution{}, fmt.Errorf("get SnapshotStorageClass %q: %w", name, err)
	}
	return classUsability(class), nil
}

// classUsability says whether new content can be bound through the class.
func classUsability(class *snapshotv1alpha1.SnapshotStorageClass) storageClassResolution {
	if !class.DeletionTimestamp.IsZero() {
		return storageClassResolution{
			Class:   class,
			Reason:  ReasonStorageClassTerminating,
			Message: fmt.Sprintf("class %q is being deleted", class.Name),
		}
	}
	cond := meta.FindStatusCondition(class.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || class.Status.StoreID == "" {
		message := fmt.Sprintf("class %q is not ready", class.Name)
		if cond != nil {
			message = fmt.Sprintf("class %q is not ready: %s: %s", class.Name, cond.Reason, cond.Message)
		}
		return storageClassResolution{Class: class, Reason: ReasonStorageClassNotReady, Message: message}
	}
	return storageClassResolution{Class: class}
}
