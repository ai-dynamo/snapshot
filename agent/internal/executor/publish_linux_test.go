// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenameNoReplacePreservesExistingArtifact(t *testing.T) {
	root := t.TempDir()
	staged := filepath.Join(root, "staged")
	final := filepath.Join(root, "final")
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(final, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "staged.img"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(final, "existing.img"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := renameNoReplace(staged, final); !os.IsExist(err) {
		t.Fatalf("renameNoReplace error = %v, want exists", err)
	}
	for _, path := range []string{filepath.Join(staged, "staged.img"), filepath.Join(final, "existing.img")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved path %s: %v", path, err)
		}
	}
}

func TestValidateCheckpointStorage(t *testing.T) {
	basePath := t.TempDir()
	if err := ValidateCheckpointStorage(basePath); err != nil {
		t.Fatalf("ValidateCheckpointStorage: %v", err)
	}
	entries, err := os.ReadDir(basePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("publication probe left entries behind: %v", entries)
	}
}
