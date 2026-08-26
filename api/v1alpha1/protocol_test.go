// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"
)

func TestGetRestoreFromSnapshotName(t *testing.T) {
	t.Run("ignores unrelated metadata", func(t *testing.T) {
		name, err := GetRestoreFromSnapshotName(map[string]string{
			RestoreFromAnnotation: "snapshot-a",
			"example.com/team":    "inference",
		})
		if err != nil || name != "snapshot-a" {
			t.Fatalf("GetRestoreFromSnapshotName() = %q, %v", name, err)
		}
	})

	for name, annotations := range map[string]map[string]string{
		"missing": {},
		"empty":   {RestoreFromAnnotation: " "},
		"invalid": {RestoreFromAnnotation: "Bad_Name"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := GetRestoreFromSnapshotName(annotations)
			if err == nil {
				t.Fatal("GetRestoreFromSnapshotName() unexpectedly succeeded")
			}
		})
	}
}
