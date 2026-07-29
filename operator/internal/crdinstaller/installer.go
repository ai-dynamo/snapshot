// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package crdinstaller applies CustomResourceDefinition manifests with
// server-side apply.
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

const FieldManager = "snapshot-crd-installer"

const crdKind = "CustomResourceDefinition"

type Client interface {
	Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error
	Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error
}

type Action string

const (
	ActionCreated   Action = "created"
	ActionUpdated   Action = "updated"
	ActionUnchanged Action = "unchanged"
)

type Result struct {
	Name   string
	Action Action
}

type Results []Result

func (r Results) Changed() bool {
	for _, res := range r {
		if res.Action != ActionUnchanged {
			return true
		}
	}
	return false
}

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

	// The server only bumps the resource version when the definition really
	// moved, which is what separates updated from unchanged below.
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

	// Apply writes the server's response back into obj.
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
