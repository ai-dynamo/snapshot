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
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralcel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	structurallisttype "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	structuralpruning "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// Use the generated schema and Kubernetes' pruning, defaulting, OpenAPI, and
// CEL implementations; the fake client does not enforce admission.
func TestSnapshotJobNonTargetArtifactPolicyAdmissionAndCompatibility(t *testing.T) {
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal([]byte(crds.SnapshotJobCRD()), &crd))
	require.Len(t, crd.Spec.Versions, 1)
	var internalCRD apiextensions.CustomResourceDefinition
	require.NoError(t, apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, &internalCRD, nil))
	internalCRD.Status.StoredVersions = []string{crd.Spec.Versions[0].Name}
	require.Empty(t, apiextensionsvalidation.ValidateCustomResourceDefinition(context.Background(), &internalCRD),
		"generated CRD must be accepted by Kubernetes")
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
				"containers": []any{map[string]any{"name": "worker", "image": "test-image"}, map[string]any{"name": "saver", "image": "test-image"}},
			}},
			"podSnapshotTemplate": map[string]any{"targetContainers": []any{"worker"}},
		},
	}
	structuraldefaulting.Default(base, structural)
	_, inserted := base["spec"].(map[string]any)["nonTargetArtifacts"]
	require.False(t, inserted, "a new default must not rewrite existing immutable specs")
	for _, tc := range []struct {
		name   string
		policy map[string]any
		valid  bool
	}{
		{"legacy omission", nil, true},
		{"implicit retain", map[string]any{"containers": []any{"saver"}}, true},
		{"explicit retain", map[string]any{"containers": []any{"saver"}, "onFailure": "Retain"}, true},
		{"cleanup", map[string]any{"containers": []any{"saver"}, "onFailure": "Cleanup"}, true},
		{"empty container list", map[string]any{"containers": []any{}, "onFailure": "Cleanup"}, false},
		{"uppercase container name", map[string]any{"containers": []any{"Saver"}, "onFailure": "Cleanup"}, false},
		{"invalid container separator", map[string]any{"containers": []any{"save_artifacts"}, "onFailure": "Cleanup"}, false},
		{"trailing separator", map[string]any{"containers": []any{"saver-"}, "onFailure": "Cleanup"}, false},
		{"container name too long", map[string]any{"containers": []any{"saver-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "onFailure": "Cleanup"}, false},
		{"duplicate container", map[string]any{"containers": []any{"saver", "saver"}, "onFailure": "Cleanup"}, false},
		{"capture target", map[string]any{"containers": []any{"worker"}, "onFailure": "Cleanup"}, true},
		{"missing container", map[string]any{"containers": []any{"missing"}, "onFailure": "Cleanup"}, true},
		{"unknown action", map[string]any{"containers": []any{"saver"}, "onFailure": "Delete"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := runtime.DeepCopyJSON(base)
			if tc.policy != nil {
				obj["spec"].(map[string]any)["nonTargetArtifacts"] = tc.policy
			}
			structuralpruning.Prune(obj, structural, true)
			structuraldefaulting.Default(obj, structural)
			errs := apivalidation.ValidateCustomResource(field.NewPath(""), obj, validator)
			listErrs := structurallisttype.ValidateListSetsAndMaps(field.NewPath(""), structural, obj)
			celErrs, _ := cel.Validate(context.Background(), field.NewPath(""), structural, obj, nil, 10000000)
			require.Equal(t, tc.valid, len(errs) == 0 && len(listErrs) == 0 && len(celErrs) == 0,
				"schema: %v, list: %v, CEL: %v", errs, listErrs, celErrs)
			if !tc.valid {
				return
			}
			updated := runtime.DeepCopyJSON(obj)
			updated["status"] = map[string]any{"sourceJobUID": "source-uid"}
			celErrs, _ = cel.Validate(context.Background(), field.NewPath(""), structural, updated, obj, 10000000)
			require.Empty(t, celErrs)
		})
	}
	optedIn := runtime.DeepCopyJSON(base)
	optedIn["spec"].(map[string]any)["nonTargetArtifacts"] = map[string]any{"containers": []any{"saver"}, "onFailure": "Cleanup"}
	legacyAnnotation := runtime.DeepCopyJSON(optedIn)
	legacyAnnotation["spec"].(map[string]any)["podTemplate"].(map[string]any)["metadata"] = map[string]any{
		"annotations": map[string]any{"nvidia.com/snapshot-non-target-artifact-containers": "saver"},
	}
	celErrs, _ := cel.Validate(context.Background(), field.NewPath(""), structural, legacyAnnotation, nil, 10000000)
	require.NotEmpty(t, celErrs, "annotation must not compete with typed policy")
	delete(legacyAnnotation["spec"].(map[string]any), "nonTargetArtifacts")
	celErrs, _ = cel.Validate(context.Background(), field.NewPath(""), structural, legacyAnnotation, nil, 10000000)
	require.Empty(t, celErrs, "annotation-only objects remain valid across a CRD upgrade")
	celErrs, _ = cel.Validate(context.Background(), field.NewPath(""), structural, optedIn, base, 10000000)
	require.NotEmpty(t, celErrs, "an existing job cannot be retroactively opted in")
	require.Contains(t, celErrs.ToAggregate().Error(), "spec is immutable")

	// An older CRD prunes the typed field. Decoding then yields retention,
	// never cleanup permission; clients must verify the persisted selection.
	oldSchema := structural.DeepCopy()
	spec := oldSchema.Properties["spec"]
	delete(spec.Properties, "nonTargetArtifacts")
	oldSchema.Properties["spec"] = spec
	structuralpruning.Prune(optedIn, oldSchema, true)
	structuraldefaulting.Default(optedIn, oldSchema)
	payload, err := json.Marshal(optedIn)
	require.NoError(t, err)
	var decoded snapshotv1alpha1.SnapshotJob
	require.NoError(t, json.Unmarshal(payload, &decoded))
	require.Nil(t, decoded.Spec.NonTargetArtifacts)
}
