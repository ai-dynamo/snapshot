// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package crdinstaller applies CustomResourceDefinition manifests with
// server-side apply.
package crdinstaller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const FieldManager = "snapshot-crd-installer"

const crdKind = "CustomResourceDefinition"

// Apply only persists the CRD; the API server registers it asynchronously.
// Vars, not consts, so tests can shrink them.
var (
	establishedPollInterval = 200 * time.Millisecond
	establishedTimeout      = 30 * time.Second
)

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
		res, err := applyCRD(ctx, cl, log, manifest)
		if err != nil {
			return results, err
		}
		log.Info("Applied CRD", "name", res.Name, "action", string(res.Action))
		results = append(results, res)
	}
	return results, nil
}

func applyCRD(ctx context.Context, cl Client, log logr.Logger, manifest string) (Result, error) {
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

	if err := waitForEstablished(ctx, cl, log, obj.GroupVersionKind(), name); err != nil {
		return Result{Name: name}, fmt.Errorf("wait for CRD %q to become established: %w", name, err)
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

// waitForEstablished blocks until the CRD's Established condition is True,
// so callers never observe a CRD that the API server has accepted but isn't
// serving yet.
func waitForEstablished(ctx context.Context, cl Client, log logr.Logger, gvk schema.GroupVersionKind, name string) error {
	var lastErr error
	var lastConditions []any
	waiting := false
	err := wait.PollUntilContextTimeout(ctx, establishedPollInterval, establishedTimeout, true,
		func(ctx context.Context) (bool, error) {
			current := &unstructured.Unstructured{}
			current.SetGroupVersionKind(gvk)
			// A transient Get failure must not abort the wait; the timeout is
			// the only terminal condition.
			if err := cl.Get(ctx, client.ObjectKey{Name: name}, current); err != nil {
				lastErr = err
				return false, nil
			}
			lastErr = nil
			lastConditions, _, _ = unstructured.NestedSlice(current.Object, "status", "conditions")
			if crdEstablished(lastConditions) {
				return true, nil
			}
			if !waiting {
				log.Info("Waiting for CRD to become established", "name", name)
				waiting = true
			}
			return false, nil
		})
	// The bare timeout says nothing about the cause; the last observation
	// distinguishes a flaky API server from e.g. a NamesAccepted conflict
	// that will never resolve.
	switch {
	case err == nil:
		return nil
	case lastErr != nil && len(lastConditions) > 0:
		return fmt.Errorf("%w (last error: %w; last observed conditions: %s)",
			err, lastErr, formatConditions(lastConditions))
	case lastErr != nil:
		return fmt.Errorf("%w (last error: %w)", err, lastErr)
	case len(lastConditions) > 0:
		return fmt.Errorf("%w (last observed conditions: %s)", err, formatConditions(lastConditions))
	default:
		return err
	}
}

func crdEstablished(conditions []any) bool {
	for _, c := range conditions {
		condition, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if condition["type"] == "Established" && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func formatConditions(conditions []any) string {
	b, err := json.Marshal(conditions)
	if err != nil {
		return fmt.Sprintf("%v", conditions)
	}
	return string(b)
}
