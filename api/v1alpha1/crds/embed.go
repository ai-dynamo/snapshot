// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package crds embeds the CustomResourceDefinitions controller-gen generates
// from the v1alpha1 types. Shipping them with the api module keeps a schema and
// the Go types it was generated from in the same artifact, so a binary can never
// install definitions it disagrees with.
package crds

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
)

// crdFS holds every generated manifest. The glob is what makes All() complete:
// a newly generated CRD is picked up without editing this file, so it cannot be
// silently left out of the set the operator installs.
//
//go:embed *.yaml
var crdFS embed.FS

var (
	//go:embed nvidia.com_podsnapshots.yaml
	podSnapshotCRD string
	//go:embed nvidia.com_podsnapshotcontents.yaml
	podSnapshotContentCRD string
)

// all is built once at startup. Reading embedded data cannot fail at runtime,
// so a failure here means the binary itself is malformed.
var all = mustLoadAll()

func mustLoadAll() []string {
	entries, err := fs.ReadDir(crdFS, ".")
	if err != nil {
		panic(fmt.Sprintf("read embedded CRDs: %v", err))
	}

	manifests := make([]string, 0, len(entries))
	for _, entry := range entries {
		data, err := crdFS.ReadFile(entry.Name())
		if err != nil {
			panic(fmt.Sprintf("read embedded CRD %q: %v", entry.Name(), err))
		}
		manifests = append(manifests, string(data))
	}
	return manifests
}

// PodSnapshotCRD returns the PodSnapshot CRD manifest.
func PodSnapshotCRD() string {
	return podSnapshotCRD
}

// PodSnapshotContentCRD returns the PodSnapshotContent CRD manifest.
func PodSnapshotContentCRD() string {
	return podSnapshotContentCRD
}

// All returns every CRD manifest owned by this module, ordered by file name.
func All() []string {
	return slices.Clone(all)
}
