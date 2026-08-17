// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crds

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
