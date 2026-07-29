// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chartCRDDir holds the copy Helm installs on a fresh release. `make generate`
// mirrors it from this package's manifests.
const chartCRDDir = "../../../charts/snapshot/crds"

func TestAllReturnsEveryEmbeddedCRD(t *testing.T) {
	all := All()
	if len(all) != 2 {
		t.Fatalf("All() returned %d manifests, want 2", len(all))
	}
	for _, want := range []string{"podsnapshots.nvidia.com", "podsnapshotcontents.nvidia.com"} {
		var found bool
		for _, manifest := range all {
			if strings.Contains(manifest, "name: "+want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("All() has no manifest for %q", want)
		}
	}
}

func TestEmbeddedManifestsAreCRDs(t *testing.T) {
	for name, manifest := range map[string]string{
		"podsnapshots.nvidia.com":        PodSnapshotCRD(),
		"podsnapshotcontents.nvidia.com": PodSnapshotContentCRD(),
	} {
		if !strings.Contains(manifest, "kind: CustomResourceDefinition") {
			t.Errorf("%s: not a CustomResourceDefinition", name)
		}
		if !strings.Contains(manifest, "name: "+name) {
			t.Errorf("%s: manifest does not declare that name", name)
		}
	}
}

// The chart carries its own copy so Helm can install CRDs on a fresh release.
// It is generated from these manifests, so any drift means someone edited one
// side by hand or skipped `make generate`.
func TestChartCopyMatchesEmbedded(t *testing.T) {
	for file, embedded := range map[string]string{
		"nvidia.com_podsnapshots.yaml":        PodSnapshotCRD(),
		"nvidia.com_podsnapshotcontents.yaml": PodSnapshotContentCRD(),
	} {
		onDisk, err := os.ReadFile(filepath.Join(chartCRDDir, file))
		if err != nil {
			t.Fatalf("read chart copy %s: %v", file, err)
		}
		if string(onDisk) != embedded {
			t.Errorf("%s differs between the chart and the embedded copy; run 'make generate'", file)
		}
	}
}
