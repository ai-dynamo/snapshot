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
	"sync"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const FieldManager = "snapshot-crd-installer"

const crdKind = "CustomResourceDefinition"

// Apply only persists the CRD; the API server registers it asynchronously.
// A var, not a const, so tests can shrink it.
var establishedTimeout = 30 * time.Second

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

func InstallCRDs(ctx context.Context, cl Client, crdClient apiextensionsv1client.CustomResourceDefinitionInterface, log logr.Logger, manifests []string) (Results, error) {
	results := make(Results, 0, len(manifests))
	for _, manifest := range manifests {
		res, err := applyCRD(ctx, cl, crdClient, log, manifest)
		if err != nil {
			return results, err
		}
		log.Info("Applied CRD", "name", res.Name, "action", string(res.Action))
		results = append(results, res)
	}
	return results, nil
}

func applyCRD(ctx context.Context, cl Client, crdClient apiextensionsv1client.CustomResourceDefinitionInterface, log logr.Logger, manifest string) (Result, error) {
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

	if err := waitForEstablished(ctx, crdClient, log, name); err != nil {
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

// waitForEstablished blocks until the CRD's Established condition is True, so
// callers never observe a CRD that the API server has accepted but isn't
// serving yet. It watches rather than polls: the initial list delivers the
// current state, updates arrive as watch events, and the underlying informer
// relists after disconnections or expired resource versions.
func waitForEstablished(ctx context.Context, crdClient apiextensionsv1client.CustomResourceDefinitionInterface, log logr.Logger, name string) error {
	ctx, cancel := context.WithTimeout(ctx, establishedTimeout)
	defer cancel()

	var (
		reflectorMu  sync.Mutex
		reflectorErr error
		failFast     error
	)
	observe := func(err error) {
		if err == nil {
			return
		}
		reflectorMu.Lock()
		reflectorErr = err
		if failFast == nil && (apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err)) {
			failFast = err
		}
		abort := failFast != nil
		reflectorMu.Unlock()
		if abort {
			cancel()
		}
	}

	lw := newCRDListWatch(crdClient, name, observe)

	// On timeout the bare error says nothing about the cause; the last
	// observation distinguishes a slow apiserver from e.g. a NamesAccepted
	// conflict that will never resolve.
	var lastConditions []apiextensionsv1.CustomResourceDefinitionCondition
	waiting := false
	_, err := watchtools.UntilWithSync(ctx, lw, &apiextensionsv1.CustomResourceDefinition{}, nil,
		func(event watch.Event) (bool, error) {
			if event.Type == watch.Deleted {
				return false, fmt.Errorf("CRD %q was deleted while waiting", name)
			}
			crd, ok := event.Object.(*apiextensionsv1.CustomResourceDefinition)
			if !ok || crd.Name != name {
				return false, nil
			}
			lastConditions = crd.Status.Conditions
			if crdEstablished(crd.Status.Conditions) {
				return true, nil
			}
			if !waiting {
				log.Info("Waiting for CRD to become established", "name", name)
				waiting = true
			}
			return false, nil
		})
	if err == nil {
		return nil
	}
	reflectorMu.Lock()
	defer reflectorMu.Unlock()
	if failFast != nil {
		return fmt.Errorf("list/watch CRDs: %w", failFast)
	}
	if reflectorErr != nil {
		err = fmt.Errorf("%w (last list/watch error: %v)", err, reflectorErr)
	}
	if len(lastConditions) > 0 {
		err = fmt.Errorf("%w (last observed conditions: %s)", err, formatConditions(lastConditions))
	}
	return err
}

// newCRDListWatch lists and watches the single named CRD, reporting every
// list/watch error to observe.
func newCRDListWatch(crdClient apiextensionsv1client.CustomResourceDefinitionInterface, name string, observe func(error)) *cache.ListWatch {
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	return &cache.ListWatch{
		ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			opts.FieldSelector = selector
			list, err := crdClient.List(ctx, opts)
			observe(err)
			return list, err
		},
		WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
			opts.FieldSelector = selector
			w, err := crdClient.Watch(ctx, opts)
			observe(err)
			return w, err
		},
	}
}

func crdEstablished(conditions []apiextensionsv1.CustomResourceDefinitionCondition) bool {
	for _, c := range conditions {
		if c.Type == apiextensionsv1.Established && c.Status == apiextensionsv1.ConditionTrue {
			return true
		}
	}
	return false
}

func formatConditions(conditions []apiextensionsv1.CustomResourceDefinitionCondition) string {
	b, err := json.Marshal(conditions)
	if err != nil {
		return fmt.Sprintf("%v", conditions)
	}
	return string(b)
}
