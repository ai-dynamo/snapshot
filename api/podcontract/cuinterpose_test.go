// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func enabledTemplate(containers ...corev1.Container) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{CuInterposeAnnotation: "true"},
		},
		Spec: corev1.PodSpec{Containers: containers},
	}
}

func testDelivery() CuInterposeDelivery {
	return CuInterposeDelivery{AgentImage: "registry.example/snapshot-agent:v1"}
}

func TestShapeCuInterposeCapture(t *testing.T) {
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

	{
		if err := ShapeCuInterposeCapture(template, []string{"worker-a", "worker-b"}, testDelivery()); err != nil {
			t.Fatalf("ShapeCuInterposeCapture() failed: %v", err)
		}
	}

	if len(template.Spec.Volumes) != 1 || template.Spec.Volumes[0].EmptyDir == nil ||
		len(template.Spec.InitContainers) != 1 {
		t.Fatalf("missing shim installation: %#v", template.Spec)
	}
	init := template.Spec.InitContainers[0]
	wantArgs := []string{
		"--",
		"/usr/local/lib/snapshot/libcuinterpose.so",
		"/usr/local/lib/snapshot/libcuinterpose_core.so",
		CuInterposeMountPath + "/",
	}
	if init.Image != testDelivery().AgentImage || !slices.Equal(init.Command, []string{"/bin/cp"}) ||
		!slices.Equal(init.Args, wantArgs) {
		t.Fatalf("incorrect installer: %#v", init)
	}
	for _, c := range template.Spec.Containers[:2] {
		if len(c.VolumeMounts) != 1 || !c.VolumeMounts[0].ReadOnly ||
			c.VolumeMounts[0].Name != cuInterposeVolumeName || c.VolumeMounts[0].MountPath != CuInterposeMountPath {
			t.Fatalf("missing read-only shim mount: %#v", c)
		}
	}
	if !reflect.DeepEqual(template.Spec.Containers[2], before.Containers[2]) {
		t.Fatal("non-target sidecar changed")
	}
	wantPreload := CuInterposeLibraryPath + " /opt/first.so /opt/second.so"
	if got := template.Spec.Containers[0].Env[0].Value; got != wantPreload {
		t.Fatalf("%s = %q, want %q", ldPreloadEnv, got, wantPreload)
	}
	wantEnv := []corev1.EnvVar{{Name: ldPreloadEnv, Value: CuInterposeLibraryPath}}
	if got := template.Spec.Containers[1].Env; !slices.Equal(got, wantEnv) {
		t.Fatalf("worker-b environment = %v, want %v", got, wantEnv)
	}
	for i := range template.Spec.Containers {
		got := &template.Spec.Containers[i]
		want := &before.Containers[i]
		if !reflect.DeepEqual(got.Command, want.Command) ||
			!reflect.DeepEqual(got.Args, want.Args) {
			t.Fatalf("container %q delivery or command changed: %#v", got.Name, got)
		}
	}
	if got := findContainer(&template.Spec, "helper").Env; len(got) != 0 {
		t.Fatalf("non-target helper environment: %#v", got)
	}

	// Without the annotation nothing is touched.
	plain := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}}}}
	plainBefore := plain.DeepCopy()
	if err := ShapeCuInterposeCapture(plain, []string{"worker"}, CuInterposeDelivery{}); err != nil ||
		!reflect.DeepEqual(plain, plainBefore) {
		t.Fatalf("disabled capture: err = %v, spec = %#v", err, plain.Spec)
	}
}

func TestShapeCuInterposeCaptureRejectsInvalidContract(t *testing.T) {
	worker := corev1.Container{Name: "worker", Command: []string{"worker"}}
	tests := map[string]struct {
		template *corev1.PodTemplateSpec
		targets  []string
	}{
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
			if err := ShapeCuInterposeCapture(tc.template, tc.targets, testDelivery()); err == nil {
				t.Fatal("ShapeCuInterposeCapture() succeeded, want error")
			}
			if !reflect.DeepEqual(tc.template, before) {
				t.Fatalf("failed shape mutated template:\ngot:  %#v\nwant: %#v", tc.template, before)
			}
		})
	}
}

func TestCuInterposeEnabled(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"true", true}, {" TRUE ", true}, {"1", true},
		{"false", false}, {"enabled", false}, {"", false},
	} {
		if got := CuInterposeEnabled(map[string]string{CuInterposeAnnotation: tc.value}); got != tc.want {
			t.Errorf("CuInterposeEnabled(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestCuInterposeDeliveryConfiguration(t *testing.T) {
	tpl := enabledTemplate(corev1.Container{Name: "worker"})
	if err := ShapeCuInterposeCapture(tpl, []string{"worker"}, CuInterposeDelivery{}); err == nil {
		t.Fatal("missing agent image accepted")
	}
	tpl.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "existing"}}
	delivery := CuInterposeDelivery{
		AgentImage: "registry.example/agent:dev",
		PullPolicy: corev1.PullNever,
	}
	if err := ShapeCuInterposeCapture(tpl, []string{"worker"}, delivery); err != nil {
		t.Fatal(err)
	}
	if tpl.Spec.InitContainers[0].ImagePullPolicy != corev1.PullNever {
		t.Fatal("configured pull policy lost")
	}
	wantSecrets := []corev1.LocalObjectReference{{Name: "existing"}}
	if !slices.Equal(tpl.Spec.ImagePullSecrets, wantSecrets) {
		t.Fatalf("pull secrets: %v", tpl.Spec.ImagePullSecrets)
	}
}
