// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testAgentImage = "registry.example/snapshot-agent@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestShapeCUDAInterposerCapture(t *testing.T) {
	template := &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{CUDAInterposerAnnotation: "enabled"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "worker-a",
					Env:  []corev1.EnvVar{{Name: ldPreloadEnv, Value: "/opt/first.so:/opt/second.so"}},
				},
				{Name: "worker-b"},
				{Name: "helper"},
			},
		},
	}

	for range 2 {
		if err := ShapeCUDAInterposerCapture(template, []string{"worker-a", "worker-b"}, testAgentImage); err != nil {
			t.Fatalf("ShapeCUDAInterposerCapture() failed: %v", err)
		}
	}

	if len(template.Spec.Volumes) != 1 ||
		template.Spec.Volumes[0].Name != CUDAInterposerVolumeName ||
		template.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("unexpected volumes: %#v", template.Spec.Volumes)
	}
	wantInit := expectedCUDAInterposerInitContainer(testAgentImage)
	if len(template.Spec.InitContainers) != 1 ||
		!reflect.DeepEqual(template.Spec.InitContainers[0], wantInit) {
		t.Fatalf("init containers = %#v, want %#v", template.Spec.InitContainers, wantInit)
	}
	for _, name := range []string{"worker-a", "worker-b"} {
		container := findContainer(&template.Spec, name)
		if got := container.VolumeMounts; len(got) != 1 ||
			got[0].Name != CUDAInterposerVolumeName ||
			got[0].MountPath != CUDAInterposerMountPath ||
			!got[0].ReadOnly {
			t.Fatalf("%s mounts: %#v", name, got)
		}
	}
	if got := findContainer(&template.Spec, "helper"); len(got.VolumeMounts) != 0 {
		t.Fatalf("non-target helper mounts: %#v", got.VolumeMounts)
	}
	wantPreload := CUDAInterposerLibraryPath + " /opt/first.so /opt/second.so"
	if got := envValue(findContainer(&template.Spec, "worker-a").Env, ldPreloadEnv); got != wantPreload {
		t.Fatalf("%s = %q, want %q", ldPreloadEnv, got, wantPreload)
	}
	if got := envValue(findContainer(&template.Spec, "worker-b").Env, ldPreloadEnv); got != CUDAInterposerLibraryPath {
		t.Fatalf("%s = %q, want %q", ldPreloadEnv, got, CUDAInterposerLibraryPath)
	}
}

func TestShapeCUDAInterposerCaptureDisabled(t *testing.T) {
	template := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}},
	}
	if err := ShapeCUDAInterposerCapture(template, []string{"worker"}, ""); err != nil {
		t.Fatalf("disabled ShapeCUDAInterposerCapture() failed: %v", err)
	}
	if len(template.Spec.Volumes) != 0 || len(template.Spec.InitContainers) != 0 {
		t.Fatalf("disabled capture was mutated: %#v", template.Spec)
	}
}

func TestShapeCUDAInterposerCaptureRejectsInvalidContract(t *testing.T) {
	tests := map[string]*corev1.PodTemplateSpec{
		"invalid annotation": {
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{CUDAInterposerAnnotation: "true"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}},
		},
		"missing agent image": {
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{CUDAInterposerAnnotation: "enabled"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}},
		},
		"unknown target": {
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{CUDAInterposerAnnotation: "enabled"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}},
		},
	}

	for name, template := range tests {
		t.Run(name, func(t *testing.T) {
			before := template.DeepCopy()
			image := testAgentImage
			targets := []string{"worker"}
			if name == "missing agent image" {
				image = ""
			}
			if name == "unknown target" {
				targets = []string{"other"}
			}
			if err := ShapeCUDAInterposerCapture(template, targets, image); err == nil {
				t.Fatal("ShapeCUDAInterposerCapture() succeeded, want error")
			}
			if !reflect.DeepEqual(template, before) {
				t.Fatalf("failed shape mutated template:\ngot:  %#v\nwant: %#v", template, before)
			}
		})
	}
}

func TestCUDAInterposerEnabled(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"absent":  nil,
		"enabled": {CUDAInterposerAnnotation: "enabled"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := CUDAInterposerEnabled(annotations)
			if err != nil {
				t.Fatalf("CUDAInterposerEnabled() failed: %v", err)
			}
			if got != (name == "enabled") {
				t.Fatalf("CUDAInterposerEnabled() = %v", got)
			}
		})
	}
	for _, value := range []string{"", "true", " enabled "} {
		if _, err := CUDAInterposerEnabled(map[string]string{CUDAInterposerAnnotation: value}); err == nil {
			t.Fatalf("CUDAInterposerEnabled() accepted %q", value)
		}
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for i := range env {
		if env[i].Name == name {
			return env[i].Value
		}
	}
	return ""
}
