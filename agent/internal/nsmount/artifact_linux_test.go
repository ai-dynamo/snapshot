// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nsmount

import (
	"os"
	"path/filepath"
	"testing"
)

func makeArtifactTree(t *testing.T) (string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(base, "artifacts", "content-uid-123", "containers", "main")
	if err := os.MkdirAll(artifact, 0o755); err != nil {
		t.Fatal(err)
	}
	return base, artifact
}

func TestResolveArtifact(t *testing.T) {
	base, artifact := makeArtifactTree(t)
	got, err := ResolveArtifact(base, "content-uid-123", "main")
	if err != nil || got != artifact {
		t.Fatalf("ResolveArtifact() = %q, %v; want %q", got, err, artifact)
	}

	for _, tc := range []struct {
		name  string
		setup func(*testing.T) string
	}{
		{"symlink", func(t *testing.T) string {
			base := t.TempDir()
			outside, _ := makeArtifactTree(t)
			if err := os.MkdirAll(filepath.Join(base, "artifacts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "artifacts", "content-uid-123"), filepath.Join(base, "artifacts", "content-uid-123")); err != nil {
				t.Fatal(err)
			}
			return base
		}},
		{"missing", func(t *testing.T) string { return t.TempDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveArtifact(tc.setup(t), "content-uid-123", "main"); err == nil {
				t.Fatal("expected artifact validation error")
			}
		})
	}
}
