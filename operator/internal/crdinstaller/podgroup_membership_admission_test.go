//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func TestPodGroupSnapshotMembershipAdmission(t *testing.T) {
	cl := startPodGroupEnv(t)
	valid := func() map[string]any {
		return map[string]any{"name": "group.example", "uid": "group-uid", "memberID": "worker-0"}
	}
	for _, kind := range []string{"PodSnapshot", "PodSnapshotContent"} {
		t.Run(kind, func(t *testing.T) {
			newObject := func(name string) *unstructured.Unstructured {
				var obj client.Object = newSettingsSnapshot(name, snapshotv1.ContainerSnapshotConfig{Name: "main"})
				if kind == "PodSnapshotContent" {
					obj = newStorageContent(name, false, "")
				}
				fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
				require.NoError(t, err)
				result := &unstructured.Unstructured{Object: fields}
				result.SetGroupVersionKind(snapshotv1.GroupVersion.WithKind(kind))
				if kind == "PodSnapshot" {
					result.SetNamespace("default")
				}
				return result
			}
			for i, tc := range []struct {
				name    string
				change  func(map[string]any)
				refusal string
			}{
				{"valid", func(map[string]any) {}, ""},
				{"maximum-lengths", func(m map[string]any) {
					m["name"] = strings.Repeat("a.", 126) + "a"
					m["uid"] = strings.Repeat("u", 128)
					m["memberID"] = strings.Repeat("m", 63)
				}, ""},
				{"missing-name", func(m map[string]any) { delete(m, "name") }, "podGroup.name"},
				{"empty-name", func(m map[string]any) { m["name"] = "" }, "podGroup.name"},
				{"invalid-name", func(m map[string]any) { m["name"] = "Group" }, "podGroup.name"},
				{"slash-name", func(m map[string]any) { m["name"] = "bad/name" }, "podGroup.name"},
				{"percent-name", func(m map[string]any) { m["name"] = "bad%name" }, "podGroup.name"},
				{"dot-name", func(m map[string]any) { m["name"] = "." }, "podGroup.name"},
				{"double-dot-name", func(m map[string]any) { m["name"] = ".." }, "podGroup.name"},
				{"long-name", func(m map[string]any) { m["name"] = strings.Repeat("a.", 126) + "aa" }, "podGroup.name"},
				{"missing-uid", func(m map[string]any) { delete(m, "uid") }, "podGroup.uid"},
				{"empty-uid", func(m map[string]any) { m["uid"] = "" }, "podGroup.uid"},
				{"long-uid", func(m map[string]any) { m["uid"] = strings.Repeat("u", 129) }, "podGroup.uid"},
				{"missing-member", func(m map[string]any) { delete(m, "memberID") }, "podGroup.memberID"},
				{"empty-member", func(m map[string]any) { m["memberID"] = "" }, "podGroup.memberID"},
				{"invalid-member", func(m map[string]any) { m["memberID"] = "worker.0" }, "podGroup.memberID"},
				{"long-member", func(m map[string]any) { m["memberID"] = strings.Repeat("m", 64) }, "podGroup.memberID"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					obj := newObject(fmt.Sprintf("membership-%d", i))
					membership := valid()
					tc.change(membership)
					require.NoError(t, unstructured.SetNestedMap(obj.Object, membership, "spec", "podGroup"))
					requireAdmission(t, cl.Create(t.Context(), obj), tc.refusal)
				})
			}
			for _, field := range []string{"name", "uid", "memberID", "remove", "add"} {
				t.Run("immutable-"+field, func(t *testing.T) {
					obj := newObject("membership-immutable-" + strings.ToLower(field))
					if field != "add" {
						require.NoError(t, unstructured.SetNestedMap(obj.Object, valid(), "spec", "podGroup"))
					}
					require.NoError(t, cl.Create(t.Context(), obj))
					changed := valid()
					switch field {
					case "remove":
						unstructured.RemoveNestedField(obj.Object, "spec", "podGroup")
					case "add":
						require.NoError(t, unstructured.SetNestedMap(obj.Object, changed, "spec", "podGroup"))
					default:
						changed[field] = "another"
						require.NoError(t, unstructured.SetNestedMap(obj.Object, changed, "spec", "podGroup"))
					}
					requireAdmission(t, cl.Update(t.Context(), obj), "spec is immutable")
				})
			}
		})
	}
}
