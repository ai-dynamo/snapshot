// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"path/filepath"
	"testing"
)

func TestResolveArtifactRoot(t *testing.T) {
	root, err := ResolveArtifactRoot("/checkpoints", "content-uid-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/checkpoints", "artifacts", "content-uid-1"); root != want {
		t.Fatalf("root = %q, want %q", root, want)
	}

	other, err := ResolveArtifactRoot("/checkpoints", "content-uid-2")
	if err != nil {
		t.Fatal(err)
	}
	if root == other {
		t.Fatalf("distinct UIDs resolved to the same root %q", root)
	}
}

func TestResolveArtifactRootRejectsUnsafeInputs(t *testing.T) {
	for name, args := range map[string][2]string{
		"relative base": {"checkpoints", "uid"},
		"root base":     {"/", "uid"},
		"unclean base":  {"/checkpoints/../tmp", "uid"},
		"empty uid":     {"/checkpoints", ""},
		"traversal uid": {"/checkpoints", ".."},
		"slash uid":     {"/checkpoints", "a/b"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveArtifactRoot(args[0], args[1])
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateArtifactContainment(t *testing.T) {
	if err := ValidateArtifactContainment("/checkpoints/artifacts", "/checkpoints/artifacts/uid"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/checkpoints/artifacts-other/uid", "/checkpoints"} {
		if err := ValidateArtifactContainment("/checkpoints/artifacts", target); err == nil {
			t.Fatalf("target %q should escape root", target)
		}
	}
}
