// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"strings"
	"testing"
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
