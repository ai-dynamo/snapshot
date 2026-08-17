// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package crds embeds the CustomResourceDefinitions controller-gen generates
// from the v1alpha1 types.
package crds

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
)

// The glob is what keeps All() complete: a newly generated CRD is picked up
// without editing this file, so it cannot be left uninstalled by omission.
//
//go:embed *.yaml
var crdFS embed.FS

var (
	//go:embed nvidia.com_podsnapshots.yaml
	podSnapshotCRD string
	//go:embed nvidia.com_podsnapshotcontents.yaml
	podSnapshotContentCRD string
	//go:embed nvidia.com_snapshotjobs.yaml
	snapshotJobCRD string
)

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

func PodSnapshotCRD() string {
	return podSnapshotCRD
}

func PodSnapshotContentCRD() string {
	return podSnapshotContentCRD
}

func SnapshotJobCRD() string {
	return snapshotJobCRD
}

func All() []string {
	return slices.Clone(all)
}
