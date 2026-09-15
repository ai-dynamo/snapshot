// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func enabledTemplate(containers ...corev1.Container) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{CuinterposeAnnotation: CuinterposeAnnotationEnabled},
		},
		Spec: corev1.PodSpec{Containers: containers},
	}
}

func TestShapeCuinterposeCapture(t *testing.T) {
	template := enabledTemplate(
		corev1.Container{
			Name:    "worker-a",
			Command: []string{"python3", "-m", "worker"},
			Args:    []string{"--rank", "0"},
			Env:     []corev1.EnvVar{{Name: ldPreloadEnv, Value: "/opt/first.so:/opt/second.so"}},
		},
		corev1.Container{Name: "worker-b", Command: []string{"/usr/bin/worker"}},
		corev1.Container{Name: "helper"},
	)
	before := template.Spec.DeepCopy()

	// Shaping twice must be a no-op the second time.
	for range 2 {
		if err := ShapeCuinterposeCapture(template, []string{"worker-a", "worker-b"}); err != nil {
			t.Fatalf("ShapeCuinterposeCapture() failed: %v", err)
		}
	}

	if !reflect.DeepEqual(template.Spec.Volumes, before.Volumes) ||
		!reflect.DeepEqual(template.Spec.InitContainers, before.InitContainers) {
		t.Fatalf("cuinterpose shaping changed CUDA tools delivery: %#v", template.Spec)
	}
	wantPreload := CuinterposeLibraryPath + " /opt/first.so /opt/second.so"
	if got := envValue(findContainer(&template.Spec, "worker-a").Env, ldPreloadEnv); got != wantPreload {
		t.Fatalf("%s = %q, want %q", ldPreloadEnv, got, wantPreload)
	}
	if got := envValue(findContainer(&template.Spec, "worker-b").Env, ldPreloadEnv); got != CuinterposeLibraryPath {
		t.Fatalf("%s = %q, want %q", ldPreloadEnv, got, CuinterposeLibraryPath)
	}
	for i := range template.Spec.Containers {
		got := &template.Spec.Containers[i]
		want := &before.Containers[i]
		if !reflect.DeepEqual(got.Command, want.Command) ||
			!reflect.DeepEqual(got.Args, want.Args) ||
			!reflect.DeepEqual(got.VolumeMounts, want.VolumeMounts) {
			t.Fatalf("container %q delivery or command changed: %#v", got.Name, got)
		}
	}
	if got := findContainer(&template.Spec, "helper").Env; len(got) != 0 {
		t.Fatalf("non-target helper environment: %#v", got)
	}
	if err := VerifyCuinterposeCapture(&template.Spec, []string{"worker-a", "worker-b"}); err != nil {
		t.Fatalf("VerifyCuinterposeCapture() on a shaped template failed: %v", err)
	}

	// Without the annotation nothing is touched.
	plain := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}}
	plainBefore := plain.DeepCopy()
	if err := ShapeCuinterposeCapture(plain, []string{"worker"}); err != nil ||
		!reflect.DeepEqual(plain, plainBefore) {
		t.Fatalf("disabled capture: err = %v, spec = %#v", err, plain.Spec)
	}
}

func TestShapeCuinterposeCaptureRejectsInvalidContract(t *testing.T) {
	worker := corev1.Container{Name: "worker", Command: []string{"worker"}}
	tests := map[string]struct {
		template *corev1.PodTemplateSpec
		targets  []string
	}{
		"invalid annotation": {
			template: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{CuinterposeAnnotation: "true"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{worker}},
			},
			targets: []string{"worker"},
		},
		"unknown target": {
			template: enabledTemplate(worker),
			targets:  []string{"other"},
		},
		"no targets": {
			template: enabledTemplate(worker),
			targets:  nil,
		},
		"LD_PRELOAD from valueFrom": {
			template: enabledTemplate(corev1.Container{
				Name: "worker", Command: []string{"worker"},
				Env: []corev1.EnvVar{{Name: ldPreloadEnv, ValueFrom: &corev1.EnvVarSource{}}},
			}),
			targets: []string{"worker"},
		},
		"duplicate LD_PRELOAD": {
			template: enabledTemplate(corev1.Container{
				Name: "worker", Command: []string{"worker"},
				Env: []corev1.EnvVar{
					{Name: ldPreloadEnv, Value: "/opt/first.so"},
					{Name: ldPreloadEnv, Value: "/opt/second.so"},
				},
			}),
			targets: []string{"worker"},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			before := tc.template.DeepCopy()
			if err := ShapeCuinterposeCapture(tc.template, tc.targets); err == nil {
				t.Fatal("ShapeCuinterposeCapture() succeeded, want error")
			}
			if !reflect.DeepEqual(tc.template, before) {
				t.Fatalf("failed shape mutated template:\ngot:  %#v\nwant: %#v", tc.template, before)
			}
		})
	}
}

func TestVerifyCuinterposeCaptureRejectsUnshapedSpecs(t *testing.T) {
	shaped := enabledTemplate(corev1.Container{Name: "worker", Command: []string{"worker"}})
	if err := ShapeCuinterposeCapture(shaped, []string{"worker"}); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*corev1.PodSpec){
		"no preload":     func(s *corev1.PodSpec) { s.Containers[0].Env = nil },
		"missing target": func(s *corev1.PodSpec) { s.Containers[0].Name = "other" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			spec := shaped.Spec.DeepCopy()
			mutate(spec)
			if err := VerifyCuinterposeCapture(spec, []string{"worker"}); err == nil {
				t.Fatal("VerifyCuinterposeCapture() accepted an unshaped spec")
			}
		})
	}
}

func TestCuinterposeEnabled(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"absent":  nil,
		"enabled": {CuinterposeAnnotation: CuinterposeAnnotationEnabled},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := CuinterposeEnabled(annotations)
			if err != nil {
				t.Fatalf("CuinterposeEnabled() failed: %v", err)
			}
			if got != (name == "enabled") {
				t.Fatalf("CuinterposeEnabled() = %v", got)
			}
		})
	}
	for _, value := range []string{"", "true", " enabled ", "Enabled"} {
		if _, err := CuinterposeEnabled(map[string]string{CuinterposeAnnotation: value}); err == nil {
			t.Fatalf("CuinterposeEnabled() accepted %q", value)
		}
	}
}
