// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

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

func TestContainerMappingsFromAnnotations(t *testing.T) {
	t.Run("defaults to captured container", func(t *testing.T) {
		got, err := ContainerMappingsFromAnnotations(nil, "main")
		if err != nil {
			t.Fatal(err)
		}
		want := []ContainerMapping{{Source: "main", Destination: "main"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mappings = %#v, want %#v", got, want)
		}
	})

	t.Run("parses one source to many destinations", func(t *testing.T) {
		got, err := ContainerMappingsFromAnnotations(map[string]string{
			RestoreContainerMapAnnotation: " main = engine-0,main=engine-1 ",
		}, "main")
		if err != nil {
			t.Fatal(err)
		}
		want := []ContainerMapping{
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
			_, err := ContainerMappingsFromAnnotations(map[string]string{
				RestoreContainerMapAnnotation: value,
			}, "main")
			if err == nil {
				t.Fatal("ContainerMappingsFromAnnotations() unexpectedly succeeded")
			}
		})
	}
}

func TestValidateContainerMappings(t *testing.T) {
	valid := []ContainerMapping{
		{Source: "main", Destination: "engine-0"},
		{Source: "main", Destination: "engine-1"},
	}
	if err := ValidateContainerMappings(valid, "main"); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		mappings []ContainerMapping
		source   string
	}{
		"empty":               {source: "main"},
		"empty source":        {mappings: []ContainerMapping{{Source: "", Destination: "engine-0"}}, source: "main"},
		"empty destination":   {mappings: []ContainerMapping{{Source: "main", Destination: ""}}, source: "main"},
		"invalid source":      {mappings: []ContainerMapping{{Source: "Main", Destination: "engine-0"}}, source: "main"},
		"invalid destination": {mappings: []ContainerMapping{{Source: "main", Destination: "Engine_0"}}, source: "main"},
		"source mismatch":     {mappings: []ContainerMapping{{Source: "worker", Destination: "engine-0"}}, source: "main"},
		"multiple sources": {
			mappings: []ContainerMapping{
				{Source: "main", Destination: "engine-0"},
				{Source: "worker", Destination: "engine-1"},
			},
			source: "main",
		},
		"duplicate destination": {
			mappings: []ContainerMapping{
				{Source: "main", Destination: "engine-0"},
				{Source: "main", Destination: "engine-0"},
			},
			source: "main",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateContainerMappings(test.mappings, test.source); err == nil {
				t.Fatal("ValidateContainerMappings() unexpectedly succeeded")
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
