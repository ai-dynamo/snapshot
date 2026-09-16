// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestAllocationStorageIsFixedBeforeProcessLaunch(t *testing.T) {
	template := enabledTemplate(corev1.Container{Name: "worker"})
	template.Annotations[CuinterposeAllocationStorageAnnotation] = "pagebroker"
	if err := ShapeCuinterposeCapture(template, []string{"worker"}); err != nil {
		t.Fatal(err)
	}
	if got := envValue(template.Spec.Containers[0].Env, CuinterposeAllocationStorageEnv); got != "pagebroker" {
		t.Fatalf("allocation storage = %q", got)
	}
	if err := ShapeCuinterposeCapture(template, []string{"worker"}); err != nil {
		t.Fatal(err)
	}
	template.Annotations[CuinterposeAllocationStorageAnnotation] = "host-carrier"
	if err := ShapeCuinterposeCapture(template, []string{"worker"}); err == nil {
		t.Fatal("accepted a conflicting launch-time allocation mode")
	}
}

func TestAllocationStorageRequiresExplicitSupportedOptIn(t *testing.T) {
	for _, test := range []struct {
		storage, shim string
		valid         bool
	}{
		{"", "", true},
		{"host-carrier", "", true},
		{"pagebroker", "enabled", true},
		{"pagebroker", "", false},
		{"future", "enabled", false},
	} {
		annotations := map[string]string{CuinterposeAllocationStorageAnnotation: test.storage}
		if test.shim != "" {
			annotations[CuinterposeAnnotation] = test.shim
		}
		_, err := CuinterposeEnabled(annotations)
		if (err == nil) != test.valid {
			t.Errorf("storage=%q shim=%q: %v", test.storage, test.shim, err)
		}
	}
}
