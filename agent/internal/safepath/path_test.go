// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package safepath

import "testing"

func TestValidateAbsolute(t *testing.T) {
	for _, value := range []string{
		"/checkpoints",
		"/mnt/pvc-123/checkpoint_1/versions/v1.2",
		"/snapshot-binaries",
	} {
		if err := ValidateAbsolute("path", value); err != nil {
			t.Errorf("ValidateAbsolute(%q): %v", value, err)
		}
	}
}

func TestValidateAbsoluteRejectsUnsafePaths(t *testing.T) {
	for _, value := range []string{
		"",
		"/",
		"relative",
		"/checkpoints/../etc",
		"/checkpoints/./artifact",
		"/checkpoints//artifact",
		"/checkpoints/",
		"/check points/artifact",
		"/checkpoints/artifact\\escape",
		"/checkpoints/$artifact",
		"/checkpoints/artifact;id",
		"/checkpoints/`artifact`",
		"/checkpoints/artifact*",
		"/checkpoints/artifact?",
		"/checkpoints/artifact\nname",
		"/checkpoints/artifact\x00name",
		"/checkpoints/é",
	} {
		t.Run(value, func(t *testing.T) {
			if err := ValidateAbsolute("path", value); err == nil {
				t.Fatalf("ValidateAbsolute(%q) succeeded", value)
			}
		})
	}
}

func TestValidateWithinUsesPathBoundary(t *testing.T) {
	if err := ValidateWithin("source", "/checkpoints", "/checkpoints/id/versions/1"); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"/etc", "/proc", "/checkpoints-other/id"} {
		if err := ValidateWithin("source", "/checkpoints", source); err == nil {
			t.Fatalf("ValidateWithin(%q) succeeded", source)
		}
	}
}
