// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"testing"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	structuralpruning "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// Use the generated schema and the API server's actual pruning, defaulting,
// OpenAPI and CEL implementations; the fake client does not enforce admission.
func TestSnapshotJobFailurePolicyAdmissionAndCompatibility(t *testing.T) {
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal([]byte(crds.SnapshotJobCRD()), &crd))
	require.Len(t, crd.Spec.Versions, 1)
	var schema apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &schema, nil))
	structural, err := structuralschema.NewStructural(&schema)
	require.NoError(t, err)
	validator, _, err := apivalidation.NewSchemaValidator(&schema)
	require.NoError(t, err)
	cel := structuralcel.NewValidator(structural, true, 1000000000)
	require.NotNil(t, cel)
	base := map[string]any{
		"apiVersion": snapshotv1alpha1.GroupVersion.String(), "kind": "SnapshotJob",
		"metadata": map[string]any{"name": "checkpoint", "namespace": "test"},
		"spec": map[string]any{
			"podTemplate": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "worker", "image": "test-image"}},
			}},
			"podSnapshotTemplate": map[string]any{"targetContainers": []any{"worker"}},
		},
	}
	structuraldefaulting.Default(base, structural)
	_, inserted := base["spec"].(map[string]any)["onFailurePolicy"]
	require.False(t, inserted, "a new default must not rewrite existing immutable specs")
	for _, tc := range []struct {
		name   string
		policy any
		valid  bool
	}{
		{"legacy omission", nil, true},
		{"explicit retain", "Retain", true},
		{"cleanup helpers", "CleanupHelpers", true},
		{"unknown rejected", "Delete", false},
		{"explicit empty rejected", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := runtime.DeepCopyJSON(base)
			if tc.policy != nil {
				obj["spec"].(map[string]any)["onFailurePolicy"] = tc.policy
			}
			structuralpruning.Prune(obj, structural, true)
			structuraldefaulting.Default(obj, structural)
			errs := apivalidation.ValidateCustomResource(field.NewPath(""), obj, validator)
			require.Equal(t, tc.valid, len(errs) == 0, "%v", errs)
			if !tc.valid {
				return
			}
			celErrs, _ := cel.Validate(context.Background(), field.NewPath(""), structural, obj, nil, 10000000)
			require.Empty(t, celErrs)
			// Status updates for old and new objects cannot enable cleanup.
			updated := runtime.DeepCopyJSON(obj)
			updated["status"] = map[string]any{"sourceJobUID": "source-uid"}
			celErrs, _ = cel.Validate(context.Background(), field.NewPath(""), structural, updated, obj, 10000000)
			require.Empty(t, celErrs)
		})
	}
	optedIn := runtime.DeepCopyJSON(base)
	optedIn["spec"].(map[string]any)["onFailurePolicy"] = "CleanupHelpers"
	errs, _ := cel.Validate(context.Background(), field.NewPath(""), structural, optedIn, base, 10000000)
	require.NotEmpty(t, errs, "an existing failed job cannot be retroactively opted in")
	require.Contains(t, errs.ToAggregate().Error(), "spec is immutable")

	// An older CRD prunes the new field. Typed decoding then yields retention,
	// not permission to delete; callers must verify the persisted selection.
	oldSchema := structural.DeepCopy()
	spec := oldSchema.Properties["spec"]
	delete(spec.Properties, "onFailurePolicy")
	oldSchema.Properties["spec"] = spec
	structuralpruning.Prune(optedIn, oldSchema, true)
	structuraldefaulting.Default(optedIn, oldSchema)
	payload, err := json.Marshal(optedIn)
	require.NoError(t, err)
	var decoded snapshotv1alpha1.SnapshotJob
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Empty(t, decoded.Spec.OnFailurePolicy)
}
