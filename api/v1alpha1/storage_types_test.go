// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestContentStorageSerializationAndDeepCopy(t *testing.T) {
	original := &PodSnapshotContent{
		Spec: PodSnapshotContentSpec{
			Storage: &CheckpointStorageBinding{StoreID: "store-v1-" + strings.Repeat("a", 64)},
		},
		Status: PodSnapshotContentStatus{
			Storage: &CheckpointStorageStatus{Artifacts: []PublishedContainerArtifact{{
				ContainerName: "main", ArtifactHandle: "opaque-handle", ArtifactFormatVersion: "snapshot.pagebroker/v1",
			}}},
		},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PodSnapshotContent
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, &decoded) {
		t.Fatal("JSON round trip changed storage binding or publication descriptors")
	}
	clone := original.DeepCopy()
	if !reflect.DeepEqual(original, clone) {
		t.Fatal("DeepCopy changed storage binding or publication descriptors")
	}
	clone.Spec.Storage.StoreID = "changed"
	clone.Status.Storage.Artifacts[0].ArtifactHandle = "changed"
	if original.Spec.Storage.StoreID != decoded.Spec.Storage.StoreID ||
		original.Status.Storage.Artifacts[0] != decoded.Status.Storage.Artifacts[0] {
		t.Fatal("DeepCopy shares mutable storage state")
	}
}

func TestLegacyContentOmitsStorage(t *testing.T) {
	content := &PodSnapshotContent{}
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"storage"`) {
		t.Fatalf("legacy serialization contains storage: %s", encoded)
	}
	var decoded PodSnapshotContent
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Spec.Storage != nil || decoded.Status.Storage != nil {
		t.Fatal("decoding legacy content invents a storage binding or publications")
	}
	clone := content.DeepCopy()
	if clone.Spec.Storage != nil || clone.Status.Storage != nil {
		t.Fatal("deep copying legacy content invents a storage binding or publications")
	}
}
