// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import "testing"

func TestResolveArtifactPath(t *testing.T) {
	got, err := ResolveArtifactPath("/checkpoints", "content-uid-123", "main")
	if err != nil || got != "/checkpoints/artifacts/content-uid-123/containers/main" {
		t.Fatalf("ResolveArtifactPath() = %q, %v", got, err)
	}
	staging, err := ResolveArtifactStagingRoot("/checkpoints", "content-uid-123")
	if err != nil || staging != "/checkpoints/artifacts/content-uid-123/.tmp" {
		t.Fatalf("ResolveArtifactStagingRoot() = %q, %v", staging, err)
	}
}

func TestResolveArtifactPathRejectsUnsafeCoordinates(t *testing.T) {
	for _, tc := range []struct {
		name, basePath, contentUID, containerName string
	}{
		{name: "relative base", basePath: "checkpoints", contentUID: "content-uid-123", containerName: "main"},
		{name: "unclean base", basePath: "/checkpoints/../etc", contentUID: "content-uid-123", containerName: "main"},
		{name: "content traversal", basePath: "/checkpoints", contentUID: "..", containerName: "main"},
		{name: "content separator", basePath: "/checkpoints", contentUID: "a/b", containerName: "main"},
		{name: "content backslash", basePath: "/checkpoints", contentUID: `a\b`, containerName: "main"},
		{name: "container traversal", basePath: "/checkpoints", contentUID: "content-uid-123", containerName: ".."},
		{name: "container separator", basePath: "/checkpoints", contentUID: "content-uid-123", containerName: "a/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveArtifactPath(tc.basePath, tc.contentUID, tc.containerName); err == nil {
				t.Fatal("expected path validation error")
			}
		})
	}
}

func TestValidateWithinRejectsUnsafeRoot(t *testing.T) {
	for _, root := range []string{"checkpoints", "/checkpoints/", "/checkpoints/../etc"} {
		t.Run(root, func(t *testing.T) {
			if err := validateWithin(root, "/checkpoints/artifacts/content-uid-123/containers/main"); err == nil {
				t.Fatal("expected root validation error")
			}
		})
	}
}
