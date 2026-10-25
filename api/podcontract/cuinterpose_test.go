// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseCuInterposeAnnotation(t *testing.T) {
	if enabled, err := ParseCuInterposeAnnotation(nil); enabled || err != nil {
		t.Fatalf("absent annotation = %v, %v", enabled, err)
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"enabled", true}, {" enabled ", true},
		{"disabled", false}, {" disabled ", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			annotations := map[string]string{CuInterposeAnnotation: tc.value}
			got, err := ParseCuInterposeAnnotation(annotations)
			if err != nil || got != tc.want {
				t.Fatalf("ParseCuInterposeAnnotation(%q) = %v, %v, want %v", tc.value, got, err, tc.want)
			}
		})
	}
	for _, value := range []string{"", " ", "true", "false", "1", "0", "ENABLED", "DISABLED", "yes"} {
		annotations := map[string]string{CuInterposeAnnotation: value}
		_, err := ParseCuInterposeAnnotation(annotations)
		if err == nil || !strings.Contains(err.Error(), CuInterposeAnnotation) {
			t.Fatalf("invalid annotation %q should identify its key: %v", value, err)
		}
	}
}

func TestValidateCuInterposeLauncher(t *testing.T) {
	for _, tc := range []struct {
		name          string
		command, args []string
		valid         bool
	}{
		{name: "workload in command", command: append(CuInterposeLauncherPrefix(), "worker"), valid: true},
		{name: "workload in args", command: CuInterposeLauncherPrefix(), args: []string{"worker"}, valid: true},
		{name: "no workload", command: CuInterposeLauncherPrefix()},
		{name: "no launcher", command: []string{"worker"}},
		{name: "another library", command: []string{CuInterposeLauncherPath, "--library", "/lib/other.so", "--", "worker"}},
		{name: "launcher in args", args: append(CuInterposeLauncherPrefix(), "worker")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			container := &corev1.Container{Name: "main", Command: tc.command, Args: tc.args}
			err := ValidateCuInterposeLauncher(container)
			if tc.valid != (err == nil) {
				t.Fatalf("ValidateCuInterposeLauncher(%q, %q) = %v, want valid %v", tc.command, tc.args, err, tc.valid)
			}
		})
	}
}
