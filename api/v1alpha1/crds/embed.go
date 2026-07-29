// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package crds embeds the CustomResourceDefinitions controller-gen generates
// from the v1alpha1 types. Shipping them with the api module keeps a schema and
// the Go types it was generated from in the same artifact, so a binary can never
// install definitions it disagrees with.
package crds

import _ "embed"

var (
	//go:embed nvidia.com_podsnapshots.yaml
	podSnapshotCRD string
	//go:embed nvidia.com_podsnapshotcontents.yaml
	podSnapshotContentCRD string
)

// PodSnapshotCRD returns the PodSnapshot CRD manifest.
func PodSnapshotCRD() string {
	return podSnapshotCRD
}

// PodSnapshotContentCRD returns the PodSnapshotContent CRD manifest.
func PodSnapshotContentCRD() string {
	return podSnapshotContentCRD
}

// All returns every CRD manifest owned by this module.
func All() []string {
	return []string{
		PodSnapshotCRD(),
		PodSnapshotContentCRD(),
	}
}
