// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveContentRoot(t *testing.T) {
	root, err := ResolveContentRoot("/checkpoints", "content-uid-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/checkpoints/artifacts/content-uid-1"; root != want {
		t.Fatalf("root = %q, want %q", root, want)
	}
}

func TestResolveContentRootRejectsUnsafeInputs(t *testing.T) {
	for _, tc := range [][2]string{{"relative", "uid"}, {"/", "uid"}, {"/checkpoints", "../uid"}} {
		if _, err := ResolveContentRoot(tc[0], tc[1]); err == nil {
			t.Fatalf("ResolveContentRoot(%q, %q) unexpectedly succeeded", tc[0], tc[1])
		}
	}
}

func TestValidateContainment(t *testing.T) {
	if err := ValidateContainment("/checkpoints/artifacts", "/checkpoints/artifacts/uid"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateContainment("/checkpoints/artifacts", "/checkpoints/other/uid"); err == nil {
		t.Fatal("expected containment validation failure")
	}
}

func TestValidateDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := ValidateDirectory(directory); err != nil {
		t.Fatalf("ValidateDirectory(%q): %v", directory, err)
	}

	missing := filepath.Join(directory, "missing")
	if err := ValidateDirectory(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ValidateDirectory(%q) error = %v, want not exists", missing, err)
	}

	file := filepath.Join(directory, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDirectory(file); err == nil {
		t.Fatal("ValidateDirectory(file) unexpectedly succeeded")
	}
}
