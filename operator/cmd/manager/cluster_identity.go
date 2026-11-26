// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
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

// listedClaimsDirectory is the directory below the mount path that holds the
// mounts of the listed claims.
const listedClaimsDirectory = ".claims"

// mountedClaims returns the mount path of a mounted claim: the claim of the
// configured PVC store at mountPath, and each listed claim in a directory of
// its name below listedClaimsDirectory.
func mountedClaims(cfg storage.Config, mountPath string, listed []string) func(string) (string, bool) {
	paths := map[string]string{}
	if mountPath != "" {
		for _, name := range listed {
			paths[name] = path.Join(mountPath, listedClaimsDirectory, name)
		}
		if cfg.PVC != nil {
			paths[cfg.PVC.ClaimName] = mountPath
		}
	}
	return func(claimName string) (string, bool) {
		mount, ok := paths[claimName]
		return mount, ok
	}
}

// parseClaimNames splits the --pvc-claim-names value.
func parseClaimNames(value string) ([]string, error) {
	var names []string
	seen := map[string]bool{}
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return nil, fmt.Errorf("claim name %q cannot be a directory below the mount path", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("claim name %q is listed twice", name)
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}

func mustClaimNames(value string) []string {
	names, err := parseClaimNames(value)
	if err != nil {
		ctrl.Log.Error(err, "invalid --pvc-claim-names")
		os.Exit(1)
	}
	return names
}
