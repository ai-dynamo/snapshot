// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/storage"
)

// kubeSystemUID returns the UID of the kube-system namespace, which identifies
// the cluster.
func kubeSystemUID(ctx context.Context, reader client.Reader) (string, error) {
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: "kube-system"}, namespace); err != nil {
		return "", fmt.Errorf("read the kube-system namespace: %w", err)
	}
	return string(namespace.UID), nil
}

// mountedClaims returns the mount path of the claim of the configured PVC
// store. Any other claim is not mounted.
func mountedClaims(cfg storage.Config, mountPath string) func(string) (string, bool) {
	return func(claimName string) (string, bool) {
		if cfg.PVC == nil || mountPath == "" || cfg.PVC.ClaimName != claimName {
			return "", false
		}
		return mountPath, true
	}
}
