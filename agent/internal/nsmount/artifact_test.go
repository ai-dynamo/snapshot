// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import "testing"

func TestResolveArtifactPath(t *testing.T) {
	for version, want := range map[string]string{
		"2": "/checkpoints/checkpoint-123/versions/2",
		"":  "/checkpoints/checkpoint-123/versions/1",
	} {
		got, err := ResolveArtifactPath("/checkpoints", "checkpoint-123", version)
		if err != nil || got != want {
			t.Fatalf("ResolveArtifactPath() = %q, %v; want %q", got, err, want)
		}
	}
}

func TestResolveArtifactPathRejectsUnsafeCoordinates(t *testing.T) {
	for _, tc := range []struct {
		name, basePath, checkpointID, version string
	}{
		{name: "relative base", basePath: "checkpoints", checkpointID: "checkpoint-123", version: "1"},
		{name: "unclean base", basePath: "/checkpoints/../etc", checkpointID: "checkpoint-123", version: "1"},
		{name: "checkpoint traversal", basePath: "/checkpoints", checkpointID: "..", version: "1"},
		{name: "checkpoint separator", basePath: "/checkpoints", checkpointID: "a/b", version: "1"},
		{name: "checkpoint backslash", basePath: "/checkpoints", checkpointID: `a\b`, version: "1"},
		{name: "version traversal", basePath: "/checkpoints", checkpointID: "checkpoint-123", version: ".."},
		{name: "version separator", basePath: "/checkpoints", checkpointID: "checkpoint-123", version: "1/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveArtifactPath(tc.basePath, tc.checkpointID, tc.version); err == nil {
				t.Fatal("expected path validation error")
			}
		})
	}
}

func TestValidateWithinRejectsUnsafeRoot(t *testing.T) {
	for _, root := range []string{"checkpoints", "/checkpoints/", "/checkpoints/../etc"} {
		t.Run(root, func(t *testing.T) {
			if err := validateWithin(root, "/checkpoints/checkpoint-123"); err == nil {
				t.Fatal("expected root validation error")
			}
		})
	}
}

func TestValidateWithinRejectsRoot(t *testing.T) {
	if err := validateWithin("/checkpoints", "/checkpoints"); err == nil {
		t.Fatal("expected root mount source validation error")
	}
}
