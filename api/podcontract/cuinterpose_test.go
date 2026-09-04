// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"strings"
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

	// Shaping twice must be a no-op the second time.
	for range 2 {
		if err := ShapeCuinterposeCapture(template, []string{"worker-a", "worker-b"}, testDelivery()); err != nil {
			t.Fatalf("ShapeCuinterposeCapture() failed: %v", err)
		}
	}

	if len(template.Spec.Volumes) != 1 ||
		template.Spec.Volumes[0].Name != CUDAToolsVolumeName ||
		template.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("unexpected volumes: %#v", template.Spec.Volumes)
	}
	wantInit := expectedCUDAToolsInitContainer(testDelivery())
	if len(template.Spec.InitContainers) != 1 ||
		!reflect.DeepEqual(template.Spec.InitContainers[0], wantInit) {
		t.Fatalf("init containers = %#v, want %#v", template.Spec.InitContainers, wantInit)
	}
	if wantInit.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("digest reference should default to IfNotPresent, got %s", wantInit.ImagePullPolicy)
	}
	if wantInit.SecurityContext.SeccompProfile == nil ||
		wantInit.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("init container lacks the RuntimeDefault seccomp profile: %#v", wantInit.SecurityContext)
	}
	for _, name := range []string{"worker-a", "worker-b"} {
		container := findContainer(&template.Spec, name)
		if got := container.VolumeMounts; len(got) != 1 ||
			got[0].Name != CUDAToolsVolumeName ||
			got[0].MountPath != CUDAToolsMountPath ||
			!got[0].ReadOnly {
			t.Fatalf("%s mounts: %#v", name, got)
		}
	}
	if got := findContainer(&template.Spec, "helper"); len(got.VolumeMounts) != 0 {
		t.Fatalf("non-target helper mounts: %#v", got.VolumeMounts)
	}
	wantPreload := CuinterposeLibraryPath + " /opt/first.so /opt/second.so"
	if got := envValue(findContainer(&template.Spec, "worker-a").Env, ldPreloadEnv); got != wantPreload {
		t.Fatalf("%s = %q, want %q", ldPreloadEnv, got, wantPreload)
	}
	if got := envValue(findContainer(&template.Spec, "worker-b").Env, ldPreloadEnv); got != CuinterposeLibraryPath {
		t.Fatalf("%s = %q, want %q", ldPreloadEnv, got, CuinterposeLibraryPath)
	}
	assertLaunchJob(
		t, findContainer(&template.Spec, "worker-a"), []string{"python3", "-m", "worker", "--rank", "0"})
	assertLaunchJob(t, findContainer(&template.Spec, "worker-b"), []string{"/usr/bin/worker"})
	if got := findContainer(&template.Spec, "helper").Command; len(got) != 0 {
		t.Fatalf("non-target helper command: %#v", got)
	}
	if err := VerifyCuinterposeCapture(&template.Spec, []string{"worker-a", "worker-b"}); err != nil {
		t.Fatalf("VerifyCuinterposeCapture() on a shaped template failed: %v", err)
	}

	// Without the annotation nothing is touched.
	plain := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}}
	if err := ShapeCuinterposeCapture(plain, []string{"worker"}, CUDAToolsDelivery{}); err != nil ||
		len(plain.Spec.Volumes) != 0 || len(plain.Spec.InitContainers) != 0 {
		t.Fatalf("disabled capture: err = %v, spec = %#v", err, plain.Spec)
	}
}

func TestShapeCuinterposeCaptureRejectsInvalidContract(t *testing.T) {
	worker := corev1.Container{Name: "worker", Command: []string{"worker"}}
	tests := map[string]struct {
		template *corev1.PodTemplateSpec
		targets  []string
		delivery CUDAToolsDelivery
	}{
		"invalid annotation": {
			template: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{CuinterposeAnnotation: "true"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{worker}},
			},
			targets: []string{"worker"}, delivery: testDelivery(),
		},
		"implicit image entrypoint": {
			template: enabledTemplate(corev1.Container{Name: "worker"}),
			targets:  []string{"worker"}, delivery: testDelivery(),
		},
		"missing agent image": {
			template: enabledTemplate(worker),
			targets:  []string{"worker"}, delivery: CUDAToolsDelivery{},
		},
		"malformed agent image": {
			template: enabledTemplate(worker),
			targets:  []string{"worker"},
			delivery: CUDAToolsDelivery{
				AgentImage: "registry.example/not valid@sha256:" + strings.Repeat("0", 64),
			},
		},
		"unknown target": {
			template: enabledTemplate(worker),
			targets:  []string{"other"}, delivery: testDelivery(),
		},
		"no targets": {
			template: enabledTemplate(worker),
			targets:  nil, delivery: testDelivery(),
		},
		"LD_PRELOAD from valueFrom": {
			template: enabledTemplate(corev1.Container{
				Name: "worker", Command: []string{"worker"},
				Env: []corev1.EnvVar{{Name: ldPreloadEnv, ValueFrom: &corev1.EnvVarSource{}}},
			}),
			targets: []string{"worker"}, delivery: testDelivery(),
		},
		"conflicting mount options": {
			template: enabledTemplate(corev1.Container{
				Name: "worker", Command: []string{"worker"},
				VolumeMounts: []corev1.VolumeMount{{Name: CUDAToolsVolumeName, MountPath: CUDAToolsMountPath, SubPath: "x"}},
			}),
			targets: []string{"worker"}, delivery: testDelivery(),
		},
		"volume is not an emptyDir": {
			template: func() *corev1.PodTemplateSpec {
				tpl := enabledTemplate(worker)
				tpl.Spec.Volumes = []corev1.Volume{{
					Name:         CUDAToolsVolumeName,
					VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp"}},
				}}
				return tpl
			}(),
			targets: []string{"worker"}, delivery: testDelivery(),
		},
		"foreign cuda-checkpoint wrapper": {
			template: enabledTemplate(corev1.Container{
				Name: "worker", Command: []string{CUDACheckpointPath}, Args: []string{"--something-else"},
			}),
			targets: []string{"worker"}, delivery: testDelivery(),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			before := tc.template.DeepCopy()
			if err := ShapeCuinterposeCapture(tc.template, tc.targets, tc.delivery); err == nil {
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
	if err := ShapeCuinterposeCapture(shaped, []string{"worker"}, testDelivery()); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*corev1.PodSpec){
		"no volume":         func(s *corev1.PodSpec) { s.Volumes = nil },
		"no init container": func(s *corev1.PodSpec) { s.InitContainers = nil },
		"no mount":          func(s *corev1.PodSpec) { s.Containers[0].VolumeMounts = nil },
		"no preload":        func(s *corev1.PodSpec) { s.Containers[0].Env = nil },
		"unwrapped command": func(s *corev1.PodSpec) {
			s.Containers[0].Command = []string{"worker"}
			s.Containers[0].Args = nil
		},
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
