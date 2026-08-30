// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const PodSnapshotContentArtifactCleanupFinalizer = "nvidia.com/podsnapshotcontent-artifact-cleanup"

var podSnapshotContentGVK = schema.GroupVersionKind{
	Group:   snapshotv1alpha1.GroupVersion.Group,
	Version: snapshotv1alpha1.GroupVersion.Version,
	Kind:    "PodSnapshotContent",
}

// SnapshotContentReconciler protects the physical artifact root with a
// metadata-only finalizer protocol.
type SnapshotContentReconciler struct {
	client.Client
	BasePath  string
	RemoveAll func(string) error
	Lstat     func(string) (os.FileInfo, error)
}

// +kubebuilder:rbac:groups=nvidia.com,resources=podsnapshotcontents,verbs=get;list;watch;patch

func (r *SnapshotContentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	content := newPodSnapshotContentMetadata()
	if err := r.Get(ctx, req.NamespacedName, content); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if content.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(content, PodSnapshotContentArtifactCleanupFinalizer) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, r.patchFinalizer(ctx, content, true)
	}
	if !controllerutil.ContainsFinalizer(content, PodSnapshotContentArtifactCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	if content.UID == "" {
		return ctrl.Result{}, fmt.Errorf("PodSnapshotContent %q has no UID", content.Name)
	}

	root, err := validateArtifactRemovalTarget(r.BasePath, string(content.UID), r.lstat())
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.removeAll()(root); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove artifact root %q: %w", root, err)
	}
	if info, err := r.lstat()(root); err == nil {
		return ctrl.Result{}, fmt.Errorf("artifact root %q still exists after removal with mode %s", root, info.Mode())
	} else if !os.IsNotExist(err) {
		return ctrl.Result{}, fmt.Errorf("confirm artifact root %q absent: %w", root, err)
	}

	if err := r.patchFinalizer(ctx, content, false); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SnapshotContentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("podsnapshotcontent-artifact-cleanup").
		For(&snapshotv1alpha1.PodSnapshotContent{}, builder.OnlyMetadata).
		Complete(r)
}

func newPodSnapshotContentMetadata() *metav1.PartialObjectMetadata {
	content := &metav1.PartialObjectMetadata{}
	content.SetGroupVersionKind(podSnapshotContentGVK)
	return content
}

type jsonPatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

func (r *SnapshotContentReconciler) patchFinalizer(ctx context.Context, content *metav1.PartialObjectMetadata, add bool) error {
	if content.UID == "" || content.ResourceVersion == "" {
		return fmt.Errorf("cannot patch finalizer for %q without UID and resource version", content.Name)
	}
	operations := []jsonPatchOperation{
		{Op: "test", Path: "/metadata/uid", Value: string(content.UID)},
		{Op: "test", Path: "/metadata/resourceVersion", Value: content.ResourceVersion},
	}
	if add {
		if len(content.Finalizers) == 0 {
			operations = append(operations, jsonPatchOperation{Op: "add", Path: "/metadata/finalizers", Value: []string{PodSnapshotContentArtifactCleanupFinalizer}})
		} else {
			operations = append(operations, jsonPatchOperation{Op: "add", Path: "/metadata/finalizers/-", Value: PodSnapshotContentArtifactCleanupFinalizer})
		}
	} else {
		index := -1
		for i, finalizer := range content.Finalizers {
			if finalizer == PodSnapshotContentArtifactCleanupFinalizer {
				index = i
				break
			}
		}
		if index == -1 {
			return nil
		}
		path := fmt.Sprintf("/metadata/finalizers/%d", index)
		operations = append(operations,
			jsonPatchOperation{Op: "test", Path: path, Value: PodSnapshotContentArtifactCleanupFinalizer},
			jsonPatchOperation{Op: "remove", Path: path},
		)
	}
	data, err := json.Marshal(operations)
	if err != nil {
		return fmt.Errorf("marshal finalizer patch: %w", err)
	}
	target := newPodSnapshotContentMetadata()
	target.Name = content.Name
	if err := r.Patch(ctx, target, client.RawPatch("application/json-patch+json", data)); err != nil {
		return fmt.Errorf("patch cleanup finalizer on PodSnapshotContent %q: %w", content.Name, err)
	}
	return nil
}

func (r *SnapshotContentReconciler) removeAll() func(string) error {
	if r.RemoveAll != nil {
		return r.RemoveAll
	}
	return os.RemoveAll
}

func (r *SnapshotContentReconciler) lstat() func(string) (os.FileInfo, error) {
	if r.Lstat != nil {
		return r.Lstat
	}
	return os.Lstat
}

func validateArtifactRemovalTarget(basePath, contentUID string, lstat func(string) (os.FileInfo, error)) (string, error) {
	artifactsRoot, err := snapshotv1alpha1.ResolveArtifactsRoot(basePath)
	if err != nil {
		return "", err
	}
	root, err := snapshotv1alpha1.ResolveArtifactRoot(basePath, contentUID)
	if err != nil {
		return "", err
	}
	for _, ancestor := range []string{basePath, artifactsRoot} {
		info, statErr := lstat(ancestor)
		if statErr != nil {
			return "", fmt.Errorf("inspect artifact ancestor %q: %w", ancestor, statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("artifact ancestor %q must be a non-symlink directory", ancestor)
		}
	}
	if info, statErr := lstat(root); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("artifact root %q must be a non-symlink directory", root)
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("inspect artifact root %q: %w", root, statErr)
	}
	return root, nil
}
