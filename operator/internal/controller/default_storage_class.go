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
	defaultClassRetry       = 5 * time.Second
)

// DefaultStorageClassCreator creates the default class from the storage
// configuration when the cluster has no class at all. That covers a fresh
// install and the first upgrade. Once any class exists it does nothing, so it
// neither brings back a deleted class nor reverts an edit.
type DefaultStorageClassCreator struct {
	Client client.Client
	// Reader reads without a cache; the manager's cache is not needed before it starts.
	Reader client.Reader
	Name   string
	Config storage.Config
}

// Start runs once, on the leader, and returns when the check is done or the context ends.
func (c *DefaultStorageClassCreator) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("default-storage-class")
	return wait.PollUntilContextCancel(ctx, defaultClassRetry, true, func(ctx context.Context) (bool, error) {
		if err := c.Ensure(ctx); err != nil {
			logger.Error(err, "Failed to check for the default SnapshotStorageClass; retrying")
			return false, nil
		}
		return true, nil
	})
}

// Ensure creates the default class when the cluster has no class.
func (c *DefaultStorageClassCreator) Ensure(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("default-storage-class")
	classes := &snapshotv1alpha1.SnapshotStorageClassList{}
	if err := c.Reader.List(ctx, classes); err != nil {
		return fmt.Errorf("list SnapshotStorageClasses: %w", err)
	}
	if len(classes.Items) > 0 {
		for i := range classes.Items {
			if classes.Items[i].Name == c.Name {
				return nil
			}
		}
		logger.Info("The default SnapshotStorageClass does not exist; PodSnapshots that name no class fail with StorageClassNotFound until it does",
			"class", c.Name)
		return nil
	}
	class := defaultClassFromConfig(c.Name, c.Config)
	if class == nil {
		logger.Info("No store is configured, so no default SnapshotStorageClass is created", "class", c.Name)
		return nil
	}
	if err := c.Client.Create(ctx, class); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create SnapshotStorageClass %q: %w", c.Name, err)
	}
	logger.Info("Created the default SnapshotStorageClass from the storage configuration", "class", c.Name)
	return nil
}

// defaultClassFromConfig describes the configured store as a class, or returns
// nil when no store is configured. Its location fields are the ones the store
// ID was computed from, so the class has the store ID already recorded on
// content bound before classes existed.
func defaultClassFromConfig(name string, cfg storage.Config) *snapshotv1alpha1.SnapshotStorageClass {
	class := &snapshotv1alpha1.SnapshotStorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       snapshotv1alpha1.SnapshotStorageClassSpec{ReclaimPolicy: snapshotv1alpha1.ReclaimPolicyDelete},
	}
	switch {
	case cfg.PVC != nil:
		class.Spec.PVC = &snapshotv1alpha1.PVCStoreSpec{ClaimName: cfg.PVC.ClaimName}
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
		class.Spec.S3 = s3
	default:
		return nil
	}
	return class
}

func keyOrDefault(key, fallback string) string {
	if key == "" {
		return fallback
	}
	return key
}
