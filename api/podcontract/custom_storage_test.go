// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCustomStorageOmitsOnlyItsOwnLaunchJob(t *testing.T) {
	for _, mode := range []string{"host-carrier", "custom-storage"} {
		template := &corev1.PodTemplateSpec{}
		template.Annotations = map[string]string{
			CuinterposeAnnotation:                  CuinterposeAnnotationEnabled,
			CuinterposeAllocationStorageAnnotation: mode,
		}
		template.Spec.Containers = []corev1.Container{{
			Name: "worker", Command: []string{"worker"},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")},
			},
		}}
		wrapped, err := ShapeCUDATools(template, []string{"worker"},
			CUDAToolsDelivery{AgentImage: "example.invalid/agent:testing"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if (len(wrapped) == 0) != (mode == "custom-storage") {
			t.Fatalf("mode %s wrapped=%v", mode, wrapped)
		}
	}
}
func TestCustomStorageRequiresInterposition(t *testing.T) {
	annotations := map[string]string{CuinterposeAllocationStorageAnnotation: "custom-storage"}
	if _, err := CuinterposeEnabled(annotations); err == nil {
		t.Fatal("accepted CustomStorage without interposition")
	}
	annotations[CuinterposeAnnotation] = CuinterposeAnnotationEnabled
	template := &corev1.PodTemplateSpec{}
	template.Annotations = annotations
	template.Spec.Containers = []corev1.Container{{Name: "worker", Command: []string{"worker"}}}
	if err := ShapeCuinterposeCapture(template, []string{"worker"}); err != nil {
		t.Fatal(err)
	}
	if got := envValue(template.Spec.Containers[0].Env, CuinterposeAllocationStorageEnv); got != "host-carrier" {
		t.Fatalf("shared content storage = %q, want host-carrier", got)
	}
}
