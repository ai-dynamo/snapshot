// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package crdinstaller applies CustomResourceDefinition manifests with
// server-side apply. Helm only creates the chart's crds/ directory on a fresh
// install and leaves it untouched on `helm upgrade`, so the operator ships an
// init container that reconciles the definitions on every rollout.
package crdinstaller

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

// LoadDir reads every YAML document from the .yaml/.yml files in dir, ordered by
// file name. Other entries are ignored, which keeps the ConfigMap mount's
// `..data` symlinks out of the way.
func LoadDir(dir string) ([][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read CRD directory %q: %w", dir, err)
	}

	var docs [][]byte
	for _, entry := range entries {
		if entry.IsDir() || !isYAML(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", path, err)
		}
		fileDocs, err := splitDocuments(data)
		if err != nil {
			return nil, fmt.Errorf("split %q: %w", path, err)
		}
		docs = append(docs, fileDocs...)
	}
	return docs, nil
}

// InstallCRDs applies every manifest in docs and reports what each apply
// changed. Applying a definition the cluster already has is a no-op.
func InstallCRDs(ctx context.Context, cl Client, log logr.Logger, docs [][]byte) ([]Result, error) {
	results := make([]Result, 0, len(docs))
	for _, doc := range docs {
		res, err := applyCRD(ctx, cl, doc)
		if err != nil {
			return results, err
		}
		log.Info("Applied CRD", "name", res.Name, "action", string(res.Action))
		results = append(results, res)
	}
	return results, nil
}

// Changed reports whether any apply in results created or updated a CRD.
func Changed(results []Result) bool {
	for _, res := range results {
		if res.Action != ActionUnchanged {
			return true
		}
	}
	return false
}

func applyCRD(ctx context.Context, cl Client, doc []byte) (Result, error) {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(doc, &obj.Object); err != nil {
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

func splitDocuments(data []byte) ([][]byte, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs [][]byte
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		docs = append(docs, doc)
	}
}

func isYAML(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yaml", ".yml":
		return true
	default:
		return false
	}
}
