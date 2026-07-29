// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package crdinstaller applies CustomResourceDefinition manifests with
// server-side apply. Helm only creates the chart's crds/ directory on a fresh
// install and leaves it untouched on `helm upgrade`, so the operator ships an
// init container that reconciles the definitions on every rollout.
package crdinstaller

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// FieldManager owns the CRD fields written by this installer. Server-side apply
// keeps ownership stable across rollouts so repeated applies converge instead of
// fighting whatever created the CRDs first.
const FieldManager = "snapshot-crd-installer"

const crdKind = "CustomResourceDefinition"

// Client is the subset of client.Client the installer needs. Narrowing it keeps
// the apply path testable without a live API server.
type Client interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error
}

// Action describes what applying a CRD changed on the server.
type Action string

const (
	// ActionCreated means the cluster had no such CRD.
	ActionCreated Action = "created"
	// ActionUpdated means the apply changed the stored definition.
	ActionUpdated Action = "updated"
	// ActionUnchanged means the stored definition already matched.
	ActionUnchanged Action = "unchanged"
)

// Result reports the outcome of applying a single CRD.
type Result struct {
	Name   string
	Action Action
}

// Results is the outcome of a full installer run.
type Results []Result

// Changed reports whether the run created or updated any CRD.
func (r Results) Changed() bool {
	for _, res := range r {
		if res.Action != ActionUnchanged {
			return true
		}
	}
	return false
}

// InstallCRDs applies every manifest and reports what each apply changed.
// Applying a definition the cluster already has is a no-op.
func InstallCRDs(ctx context.Context, cl Client, log logr.Logger, manifests []string) (Results, error) {
	results := make(Results, 0, len(manifests))
	for _, manifest := range manifests {
		res, err := applyCRD(ctx, cl, manifest)
		if err != nil {
			return results, err
		}
		log.Info("Applied CRD", "name", res.Name, "action", string(res.Action))
		results = append(results, res)
	}
	return results, nil
}

func applyCRD(ctx context.Context, cl Client, manifest string) (Result, error) {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(manifest), &obj.Object); err != nil {
		return Result{}, fmt.Errorf("unmarshal CRD manifest: %w", err)
	}
	if kind := obj.GetKind(); kind != crdKind {
		return Result{}, fmt.Errorf("expected kind %s, got %q", crdKind, kind)
	}
	name := obj.GetName()
	if name == "" {
		return Result{}, errors.New("CRD manifest has no metadata.name")
	}

	// Read the stored resource version first so an apply that changes nothing is
	// reported as such: the server only bumps it when the definition really moved.
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(obj.GroupVersionKind())
	existed := true
	err := cl.Get(ctx, client.ObjectKey{Name: name}, current)
	switch {
	case apierrors.IsNotFound(err):
		existed = false
	case err != nil:
		return Result{Name: name}, fmt.Errorf("get CRD %q: %w", name, err)
	}

	// Apply writes the server's response back into obj, so its resource version
	// after this call is the post-apply one.
	if err := cl.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
		client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return Result{Name: name}, fmt.Errorf("apply CRD %q: %w", name, err)
	}

	switch {
	case !existed:
		return Result{Name: name, Action: ActionCreated}, nil
	case current.GetResourceVersion() == obj.GetResourceVersion():
		return Result{Name: name, Action: ActionUnchanged}, nil
	default:
		return Result{Name: name, Action: ActionUpdated}, nil
	}
}
