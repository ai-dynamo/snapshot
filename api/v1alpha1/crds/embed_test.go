// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crds

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const chartCRDDir = "../../../charts/snapshot/crds"

func readCRDDir(t *testing.T, dir string) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	manifests := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		manifests[entry.Name()] = string(data)
	}
	return manifests
}

func TestAllCoversEveryGeneratedCRD(t *testing.T) {
	generated := readCRDDir(t, ".")
	if len(generated) == 0 {
		t.Fatal("no generated CRDs found; run 'make generate'")
	}

	all := All()
	if len(all) != len(generated) {
		t.Errorf("All() returns %d manifests but %d CRDs are generated", len(all), len(generated))
	}
	for name, manifest := range generated {
		if !slices.Contains(all, manifest) {
			t.Errorf("%s is generated but missing from All()", name)
		}
	}
}

func TestChartCopyMatchesGenerated(t *testing.T) {
	generated := readCRDDir(t, ".")
	chart := readCRDDir(t, chartCRDDir)

	for name, want := range generated {
		got, ok := chart[name]
		switch {
		case !ok:
			t.Errorf("%s is generated but absent from the chart; run 'make generate'", name)
		case got != want:
			t.Errorf("%s differs between the chart and this package; run 'make generate'", name)
		}
	}
	for name := range chart {
		if _, ok := generated[name]; !ok {
			t.Errorf("%s is in the chart but no longer generated; run 'make generate'", name)
		}
	}
}

func TestNamedAccessorsAreEmbedded(t *testing.T) {
	for name, manifest := range map[string]string{
		"podsnapshots.nvidia.com":        PodSnapshotCRD(),
		"podsnapshotcontents.nvidia.com": PodSnapshotContentCRD(),
		"snapshotjobs.nvidia.com":        SnapshotJobCRD(),
	} {
		if !strings.Contains(manifest, "kind: CustomResourceDefinition") {
			t.Errorf("%s: not a CustomResourceDefinition", name)
		}
		if !strings.Contains(manifest, "name: "+name) {
			t.Errorf("%s: manifest does not declare that name", name)
		}
		if !slices.Contains(All(), manifest) {
			t.Errorf("%s: accessor returns a manifest that is not in All()", name)
		}
	}
}

func TestAllReturnsACopy(t *testing.T) {
	first := All()
	if len(first) == 0 {
		t.Fatal("All() is empty")
	}
	first[0] = "mutated"

	if All()[0] == "mutated" {
		t.Error("All() exposes its backing array; callers can corrupt the embedded set")
	}
}

func TestSnapshotJobConditionsUseMapListSchema(t *testing.T) {
	manifestJSON, err := utilyaml.ToJSON([]byte(SnapshotJobCRD()))
	if err != nil {
		t.Fatalf("convert SnapshotJob CRD to JSON: %v", err)
	}
	var crd map[string]any
	if err := json.Unmarshal(manifestJSON, &crd); err != nil {
		t.Fatalf("decode SnapshotJob CRD: %v", err)
	}

	versions := nestedSlice(t, crd, "spec", "versions")
	if len(versions) == 0 {
		t.Fatal("SnapshotJob CRD has no versions")
	}
	version, ok := versions[0].(map[string]any)
	if !ok {
		t.Fatalf("SnapshotJob CRD version has type %T, want object", versions[0])
	}
	conditions := nestedMap(t, version, "schema", "openAPIV3Schema", "properties", "status", "properties", "conditions")

	if got := conditions["x-kubernetes-list-type"]; got != "map" {
		t.Errorf("conditions x-kubernetes-list-type = %v, want map", got)
	}
	keys, ok := conditions["x-kubernetes-list-map-keys"].([]any)
	if !ok || len(keys) != 1 || keys[0] != "type" {
		t.Errorf("conditions x-kubernetes-list-map-keys = %v, want [type]", conditions["x-kubernetes-list-map-keys"])
	}
}

func nestedMap(t *testing.T, object map[string]any, fields ...string) map[string]any {
	t.Helper()
	current := object
	for _, field := range fields {
		next, ok := current[field].(map[string]any)
		if !ok {
			t.Fatalf("field %q in path %q has type %T, want object", field, strings.Join(fields, "."), current[field])
		}
		current = next
	}
	return current
}

func nestedSlice(t *testing.T, object map[string]any, fields ...string) []any {
	t.Helper()
	parent := nestedMap(t, object, fields[:len(fields)-1]...)
	field := fields[len(fields)-1]
	value, ok := parent[field].([]any)
	if !ok {
		t.Fatalf("field %q in path %q has type %T, want array", field, strings.Join(fields, "."), parent[field])
	}
	return value
}
