// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/storage"
)

// pageBrokerS3Config is the JSON shape PageBroker's --storage-config reads
// for the S3 backend (agent/pagebroker/s3_config.cpp's ParseS3Config).
// storeId is computed here from the same canonical fields api/storage.S3's
// StoreID hashes: PageBroker trusts the value rather than deriving it
// itself, unlike the PVC backend's ComputeStoreID.
type pageBrokerS3Config struct {
	StoreID  string `json:"storeId"`
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix,omitempty"`
}

// PageBrokerS3ConfigKey is the ConfigMap data key the chart mounts into the
// PageBroker container at --storage-config.
const PageBrokerS3ConfigKey = "pagebroker-s3-config.json"

// ensurePageBrokerS3Config creates or updates the ConfigMap PageBroker's
// DaemonSet mounts when the S3 backend is configured. It is idempotent and
// safe to call on every manager startup. The chart does not render this
// ConfigMap itself: its storeId field requires the Go identity computation
// PageBroker does not perform for S3 (see api/storage.S3.StoreID).
func ensurePageBrokerS3Config(ctx context.Context, c client.Client, namespace, name string, s3 storage.S3, storeID string) error {
	body, err := json.Marshal(pageBrokerS3Config{
		StoreID:  storeID,
		Endpoint: s3.Endpoint,
		Region:   s3.Region,
		Bucket:   s3.Bucket,
		Prefix:   s3.Prefix,
	})
	if err != nil {
		return fmt.Errorf("encode PageBroker S3 configuration: %w", err)
	}

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{PageBrokerS3ConfigKey: string(body)},
	}
	existing := &corev1.ConfigMap{}
	switch err := c.Get(ctx, client.ObjectKeyFromObject(desired), existing); {
	case apierrors.IsNotFound(err):
		return c.Create(ctx, desired)
	case err != nil:
		return fmt.Errorf("get PageBroker S3 configuration: %w", err)
	default:
		existing.Data = desired.Data
		return c.Update(ctx, existing)
	}
}
