// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ai-dynamo/snapshot/api/storage"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	defaultS3CredentialsKey = "credentials"
	defaultS3CABundleKey    = "ca.crt"
	defaultStoreRetry       = 5 * time.Second
)

// DefaultSnapshotStoreCreator creates the default store from the storage
// configuration when the cluster has no store. If any store exists, it does
// nothing.
type DefaultSnapshotStoreCreator struct {
	Client client.Client
	// Reader reads without a cache.
	Reader client.Reader
	Name   string
	Config storage.Config
}

// Start checks once on the leader and retries until it succeeds or the context
// ends.
func (c *DefaultSnapshotStoreCreator) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("default-snapshot-store")
	return wait.PollUntilContextCancel(ctx, defaultStoreRetry, true, func(ctx context.Context) (bool, error) {
		if err := c.Ensure(ctx); err != nil {
			logger.Error(err, "Failed to check for the default SnapshotStore; retrying")
			return false, nil
		}
		return true, nil
	})
}

// Ensure creates the default store when the cluster has no store.
func (c *DefaultSnapshotStoreCreator) Ensure(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("default-snapshot-store")
	stores := &snapshotv1alpha1.SnapshotStoreList{}
	if err := c.Reader.List(ctx, stores); err != nil {
		return fmt.Errorf("list SnapshotStores: %w", err)
	}
	if len(stores.Items) > 0 {
		for i := range stores.Items {
			if stores.Items[i].Name == c.Name {
				return nil
			}
		}
		logger.Info("The default SnapshotStore does not exist; PodSnapshots that name no store fail with StoreNotFound until it does",
			"store", c.Name)
		return nil
	}
	store := defaultStoreFromConfig(c.Name, c.Config)
	if store == nil {
		logger.Info("No store is configured, so no default SnapshotStore is created", "store", c.Name)
		return nil
	}
	if err := c.Client.Create(ctx, store); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create SnapshotStore %q: %w", c.Name, err)
	}
	logger.Info("Created the default SnapshotStore from the storage configuration", "store", c.Name)
	return nil
}

// defaultStoreFromConfig describes the configured store as a store, or returns
// nil when no store is configured. Its location fields give the store ID of the
// configured store.
func defaultStoreFromConfig(name string, cfg storage.Config) *snapshotv1alpha1.SnapshotStore {
	store := &snapshotv1alpha1.SnapshotStore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       snapshotv1alpha1.SnapshotStoreSpec{ReclaimPolicy: snapshotv1alpha1.ReclaimPolicyDelete},
	}
	switch {
	case cfg.PVC != nil:
		store.Spec.PVC = &snapshotv1alpha1.PVCStoreSpec{ClaimName: cfg.PVC.ClaimName}
	case cfg.S3 != nil:
		s3 := &snapshotv1alpha1.S3StoreSpec{
			Endpoint: cfg.S3.Endpoint, Region: cfg.S3.Region, Bucket: cfg.S3.Bucket, Prefix: cfg.S3.Prefix,
		}
		if access := cfg.S3Access; access != nil {
			s3.Addressing = access.Addressing
			s3.AllowHTTP = access.AllowHTTP
			if ref := access.CredentialsSecretRef; ref != nil && ref.Name != "" {
				s3.CredentialsSecretRef = &snapshotv1alpha1.SecretKeyReference{Name: ref.Name, Key: keyOrDefault(ref.Key, defaultS3CredentialsKey)}
			}
			if ref := access.CABundleConfigMapRef; ref != nil && ref.Name != "" {
				s3.CABundleConfigMapRef = &snapshotv1alpha1.ConfigMapKeyReference{Name: ref.Name, Key: keyOrDefault(ref.Key, defaultS3CABundleKey)}
			}
		}
		store.Spec.S3 = s3
	default:
		return nil
	}
	return store
}

func keyOrDefault(key, fallback string) string {
	if key == "" {
		return fallback
	}
	return key
}
