// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"encoding/json"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	snapshotv1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

// A kubebuilder Maximum marker takes a literal, so the ceiling is written twice:
// once per spec field and once as the constant the agent validates its own
// configuration against. Nothing but this test ties them together.
//
// The schema is walked as plain JSON rather than through the apiextensions
// types, which this module does not otherwise depend on.
func TestCheckpointRetryLimitMarkerMatchesTheConstant(t *testing.T) {
	for name, manifest := range map[string]string{
		"PodSnapshot":        crds.PodSnapshotCRD(),
		"PodSnapshotContent": crds.PodSnapshotContentCRD(),
		"SnapshotJob":        crds.SnapshotJobCRD(),
	} {
		t.Run(name, func(t *testing.T) {
			var crd map[string]any
			if err := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096).Decode(&crd); err != nil {
				t.Fatalf("decode %s CRD: %v", name, err)
			}
			found := retryLimitSchemas(crd)
			if len(found) == 0 {
				t.Fatal("no checkpointRetryLimit property in the CRD; the field or this test moved")
			}
			for _, schema := range found {
				assertBound(t, schema, "maximum", float64(snapshotv1.MaxCheckpointRetryLimit))
				assertBound(t, schema, "minimum", 0)
			}
		})
	}
}

func assertBound(t *testing.T, schema map[string]any, bound string, want float64) {
	t.Helper()
	got, ok := schema[bound]
	if !ok {
		t.Errorf("checkpointRetryLimit has no %s; want %v", bound, want)
		return
	}
	// The decoder yields float64 for a plain document and json.Number when a
	// caller asked for one, so accept either rather than depend on which.
	var value float64
	switch typed := got.(type) {
	case float64:
		value = typed
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			t.Errorf("checkpointRetryLimit %s is %q, not a number", bound, typed)
			return
		}
		value = parsed
	default:
		t.Errorf("checkpointRetryLimit %s is %T, not a number", bound, got)
		return
	}
	if value != want {
		t.Errorf("checkpointRetryLimit %s is %v, want %v", bound, value, want)
	}
}

// retryLimitSchemas collects every checkpointRetryLimit anywhere in the
// document, so a field added on another path is bounded too rather than
// silently skipped.
func retryLimitSchemas(node any) []map[string]any {
	var found []map[string]any
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			if key == "checkpointRetryLimit" {
				if schema, ok := value.(map[string]any); ok {
					found = append(found, schema)
					continue
				}
			}
			found = append(found, retryLimitSchemas(value)...)
		}
	case []any:
		for _, value := range typed {
			found = append(found, retryLimitSchemas(value)...)
		}
	}
	return found
}
