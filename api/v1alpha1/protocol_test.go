// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"reflect"
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

func TestRestoreContainerMappingsFromAnnotations(t *testing.T) {
	t.Run("defaults to captured container", func(t *testing.T) {
		got, err := RestoreContainerMappingsFromAnnotations(nil, "main")
		if err != nil {
			t.Fatal(err)
		}
		want := []RestoreContainerMapping{{Source: "main", Destination: "main"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mappings = %#v, want %#v", got, want)
		}
	})

	t.Run("parses one source to many destinations", func(t *testing.T) {
		got, err := RestoreContainerMappingsFromAnnotations(map[string]string{
			RestoreContainerMapAnnotation: " main = engine-0,main=engine-1 ",
		}, "main")
		if err != nil {
			t.Fatal(err)
		}
		want := []RestoreContainerMapping{
			{Source: "main", Destination: "engine-0"},
			{Source: "main", Destination: "engine-1"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mappings = %#v, want %#v", got, want)
		}
	})

	for name, value := range map[string]string{
		"empty":           "",
		"missing equals":  "main",
		"too many equals": "main=engine=0",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := RestoreContainerMappingsFromAnnotations(map[string]string{
				RestoreContainerMapAnnotation: value,
			}, "main")
			if err == nil {
				t.Fatal("RestoreContainerMappingsFromAnnotations() unexpectedly succeeded")
			}
		})
	}
}

func TestValidateRestoreContainerMappings(t *testing.T) {
	valid := []RestoreContainerMapping{
		{Source: "main", Destination: "engine-0"},
		{Source: "main", Destination: "engine-1"},
	}
	if err := ValidateRestoreContainerMappings(valid, "main"); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		mappings []RestoreContainerMapping
		source   string
	}{
		"empty":                 {source: "main"},
		"empty source":          {mappings: []RestoreContainerMapping{{Source: "", Destination: "engine-0"}}, source: "main"},
		"empty destination":     {mappings: []RestoreContainerMapping{{Source: "main", Destination: ""}}, source: "main"},
		"invalid source":        {mappings: []RestoreContainerMapping{{Source: "Main", Destination: "engine-0"}}, source: "main"},
		"invalid destination":   {mappings: []RestoreContainerMapping{{Source: "main", Destination: "Engine_0"}}, source: "main"},
		"source mismatch":       {mappings: []RestoreContainerMapping{{Source: "worker", Destination: "engine-0"}}, source: "main"},
		"multiple sources":      {mappings: []RestoreContainerMapping{{Source: "main", Destination: "engine-0"}, {Source: "worker", Destination: "engine-1"}}, source: "main"},
		"duplicate destination": {mappings: []RestoreContainerMapping{{Source: "main", Destination: "engine-0"}, {Source: "main", Destination: "engine-0"}}, source: "main"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateRestoreContainerMappings(test.mappings, test.source); err == nil {
				t.Fatal("ValidateRestoreContainerMappings() unexpectedly succeeded")
			}
		})
	}
}
