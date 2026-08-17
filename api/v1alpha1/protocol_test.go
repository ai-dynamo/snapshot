// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateCaptureAnnotations(t *testing.T) {
	if err := ValidateCaptureAnnotations(nil); err != nil {
		t.Fatalf("nil annotations: %v", err)
	}
	if err := ValidateCaptureAnnotations(map[string]string{
		"linkerd.io/inject": "disabled",
		"example.com/team":  "inference",
	}); err != nil {
		t.Fatalf("unrelated annotations: %v", err)
	}

	for _, annotations := range []map[string]string{
		{RestoreFromAnnotation: "snapshot-a"},
		{"nvidia.com/snapshot-target-containers": "main"},
	} {
		err := ValidateCaptureAnnotations(annotations)
		if err == nil || !strings.Contains(err.Error(), "must not carry snapshot annotations") {
			t.Fatalf("ValidateCaptureAnnotations(%v) error = %v", annotations, err)
		}
	}
}

func TestRestoreFromAnnotations(t *testing.T) {
	t.Run("only snapshot annotation with unrelated metadata", func(t *testing.T) {
		name, err := RestoreFromAnnotations(map[string]string{
			RestoreFromAnnotation: "snapshot-a",
			"example.com/team":    "inference",
		})
		if err != nil || name != "snapshot-a" {
			t.Fatalf("RestoreFromAnnotations() = %q, %v", name, err)
		}
	})

	for name, annotations := range map[string]map[string]string{
		"missing": {},
		"empty":   {RestoreFromAnnotation: " "},
		"invalid": {RestoreFromAnnotation: "Bad_Name"},
		"legacy extra": {
			RestoreFromAnnotation:                  "snapshot-a",
			"nvidia.com/snapshot-artifact-version": "1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := RestoreFromAnnotations(annotations)
			if err == nil {
				t.Fatal("RestoreFromAnnotations() unexpectedly succeeded")
			}
		})
	}
}

func TestSnapshotAnnotationsSorted(t *testing.T) {
	want := []string{
		RestoreFromAnnotation,
		"nvidia.com/snapshot-z",
	}
	got := SnapshotAnnotations(map[string]string{
		"nvidia.com/snapshot-z": "x",
		RestoreFromAnnotation:   "snapshot-a",
		"example.com/a":         "y",
	})
	if !slices.Equal(got, want) {
		t.Fatalf("SnapshotAnnotations() = %v, want %v", got, want)
	}
}
