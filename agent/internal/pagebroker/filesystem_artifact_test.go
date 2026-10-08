// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"strings"
	"testing"
)

func TestFilesystemArtifactMatchesTheStoreLayout(t *testing.T) {
	storeID := "store-v1-" + strings.Repeat("a", 64)
	artifact := FilesystemArtifact(storeID, "11111111-1111-4111-8111-111111111111", "main")
	if artifact.GetStoreId() != storeID {
		t.Fatalf("store ID = %q", artifact.GetStoreId())
	}
	if artifact.GetArtifactHandle() != "artifacts/11111111-1111-4111-8111-111111111111/containers/main" {
		t.Fatalf("handle = %q", artifact.GetArtifactHandle())
	}
	if artifact.GetArtifactFormatVersion() != "snapshot.pagebroker/v1" {
		t.Fatalf("version = %q", artifact.GetArtifactFormatVersion())
	}
	if err := validatePublishedArtifact(artifact); err != nil {
		t.Fatal(err)
	}
}
