// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"
)

func TestGetRestoreFromSnapshotName(t *testing.T) {
	t.Run("ignores unrelated metadata", func(t *testing.T) {
		name, err := GetRestoreFromSnapshotName(map[string]string{
			RestoreFromAnnotation: "snapshot-a",
			"example.com/team":    "inference",
		})
		if err != nil || name != "snapshot-a" {
			t.Fatalf("GetRestoreFromSnapshotName() = %q, %v", name, err)
		}
	})

	for name, annotations := range map[string]map[string]string{
		"missing": {},
		"empty":   {RestoreFromAnnotation: " "},
		"invalid": {RestoreFromAnnotation: "Bad_Name"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := GetRestoreFromSnapshotName(annotations)
			if err == nil {
				t.Fatal("GetRestoreFromSnapshotName() unexpectedly succeeded")
			}
		})
	}
}

func TestSkipCompatCheckFromAnnotations(t *testing.T) {
	cases := map[string]bool{
		"true":  true,
		"True":  true,
		"1":     true,
		"false": false,
		"0":     false,
		" true": true,
		// A value nobody parses as a boolean leaves the gate on. Turning it off
		// by accident is the expensive direction: a restore that should have
		// been refused instead fails somewhere inside CRIU.
		"yes":         false,
		"":            false,
		"TRUE-ISH":    false,
		"true please": false,
	}
	for value, want := range cases {
		annotations := map[string]string{SkipCompatCheckAnnotation: value}
		if got := SkipCompatCheckFromAnnotations(annotations); got != want {
			t.Errorf("SkipCompatCheckFromAnnotations(%q) = %v, want %v", value, got, want)
		}
	}

	if SkipCompatCheckFromAnnotations(nil) {
		t.Error("an unannotated pod asked to skip the compatibility gate")
	}
}
