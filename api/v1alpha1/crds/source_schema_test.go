// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crds

import (
	"encoding/json"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func objectAt(t *testing.T, node map[string]any, keys ...string) map[string]any {
	t.Helper()

	for _, key := range keys {
		next, ok := node[key].(map[string]any)
		if !ok {
			t.Fatalf("generated CRD has no object at %q", key)
		}
		node = next
	}
	return node
}

// sourceSchema returns the status.source schema of the generated PodSnapshotContent CRD.
func sourceSchema(t *testing.T) map[string]any {
	t.Helper()

	asJSON, err := utilyaml.ToJSON([]byte(PodSnapshotContentCRD()))
	if err != nil {
		t.Fatalf("convert generated CRD to JSON: %v", err)
	}
	var crd map[string]any
	if err := json.Unmarshal(asJSON, &crd); err != nil {
		t.Fatalf("decode generated CRD: %v", err)
	}

	versions, ok := objectAt(t, crd, "spec")["versions"].([]any)
	if !ok {
		t.Fatal("generated CRD declares no versions")
	}
	for _, entry := range versions {
		version, ok := entry.(map[string]any)
		if !ok || version["name"] != "v1alpha1" {
			continue
		}
		schema := objectAt(t, version, "schema", "openAPIV3Schema")
		return objectAt(t, schema, "properties", "status", "properties", "source")
	}

	t.Fatal("generated CRD has no v1alpha1 version")
	return nil
}

func TestSourceTypeAndPayloadMustAgree(t *testing.T) {
	const (
		wantRule    = "(self.type == 'Nvidia') == has(self.nvidia)"
		wantMessage = "source payload must match type"
	)

	validations, ok := sourceSchema(t)["x-kubernetes-validations"].([]any)
	if !ok {
		t.Fatal("status.source carries no x-kubernetes-validations, so the server would accept a payload that contradicts its type")
	}

	for _, entry := range validations {
		validation, ok := entry.(map[string]any)
		if !ok || validation["rule"] != wantRule {
			continue
		}
		if got := validation["message"]; got != wantMessage {
			t.Errorf("rule message is %v, want %q", got, wantMessage)
		}
		return
	}

	t.Errorf("status.source has no validation with rule %q, got %v", wantRule, validations)
}

func TestSourceTypeIsAnEnumWithADefault(t *testing.T) {
	sourceType := objectAt(t, sourceSchema(t), "properties", "type")

	if got := sourceType["default"]; got != "Nvidia" {
		t.Errorf("type default is %v, want Nvidia, so a payload written without a type would be rejected", got)
	}

	enum, ok := sourceType["enum"].([]any)
	if !ok {
		t.Fatal("type is not constrained to an enum, so any vendor name would be accepted")
	}
	if len(enum) != 1 || enum[0] != "Nvidia" {
		t.Errorf("type enum is %v, want exactly [Nvidia]", enum)
	}
}
