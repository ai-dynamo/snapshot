// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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
		if preload := strings.Fields(c.Env[0].Value)[0]; !path.IsAbs(preload) {
			t.Fatalf("container %q shim preload must be absolute: %q", c.Name, preload)
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

func TestCuInterposeInstallerSecurityContext(t *testing.T) {
	for _, tc := range []struct {
		name     string
		security *corev1.PodSecurityContext
		wantUID  int64
	}{
		{name: "default", wantUID: 65532},
		{
			name: "nonroot without UID or fsGroup", wantUID: 65532,
			security: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)},
		},
		{
			name: "explicit nonroot UID", wantUID: 1000,
			security: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)},
		},
		{
			name: "root workload", wantUID: 65532,
			security: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](0)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := enabledTemplate(corev1.Container{
				Name: "worker", SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To[int64](2000)},
			})
			template.Spec.SecurityContext = tc.security
			before := template.DeepCopy()
			if err := ShapeCuInterposeCapture(template, []string{"worker"}, testDelivery()); err != nil {
				t.Fatal(err)
			}
			security := template.Spec.InitContainers[0].SecurityContext
			if security == nil || security.RunAsUser == nil || *security.RunAsUser != tc.wantUID ||
				security.RunAsNonRoot == nil || !*security.RunAsNonRoot {
				t.Fatalf("installer must use nonroot UID %d: %#v", tc.wantUID, security)
			}
			if !reflect.DeepEqual(template.Spec.SecurityContext, before.Spec.SecurityContext) {
				t.Fatal("installer changed Pod security context")
			}
			if !reflect.DeepEqual(
				template.Spec.Containers[0].SecurityContext, before.Spec.Containers[0].SecurityContext,
			) {
				t.Fatal("installer changed workload security context")
			}
		})
	}
}

func TestCuInterposePreloadWithEnvFrom(t *testing.T) {
	configMap := corev1.EnvFromSource{
		ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "workload-env"}},
	}
	secret := corev1.EnvFromSource{
		SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "workload-env"}},
	}
	for _, tc := range []struct {
		name      string
		source    corev1.EnvFromSource
		prefix    string
		explicit  *string
		wantError bool
		wantValue string
	}{
		{name: "ambiguous ConfigMap", source: configMap, wantError: true},
		{name: "ambiguous Secret", source: secret, wantError: true},
		{name: "prefix can form preload", source: configMap, prefix: "LD_", wantError: true},
		{name: "unrelated prefix", source: configMap, prefix: "APP_", wantValue: CuInterposeLibraryPath},
		{
			name: "prefix requires empty key", source: configMap,
			prefix: ldPreloadEnv, wantValue: CuInterposeLibraryPath,
		},
		{
			name: "explicit list", source: configMap, explicit: ptr.To("/opt/first.so:/opt/second.so"),
			wantValue: CuInterposeLibraryPath + " /opt/first.so /opt/second.so",
		},
		{name: "explicit empty", source: secret, explicit: ptr.To(""), wantValue: CuInterposeLibraryPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := tc.source
			source.Prefix = tc.prefix
			worker := corev1.Container{Name: "worker", EnvFrom: []corev1.EnvFromSource{source}}
			if tc.explicit != nil {
				worker.Env = []corev1.EnvVar{{Name: ldPreloadEnv, Value: *tc.explicit}}
			}
			template := enabledTemplate(worker)
			before := template.DeepCopy()
			err := ShapeCuInterposeCapture(template, []string{"worker"}, testDelivery())
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "define LD_PRELOAD explicitly") {
					t.Fatalf("want explicit-preload error, got %v", err)
				}
				if !reflect.DeepEqual(template, before) {
					t.Fatal("rejected preload configuration changed the template")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := template.Spec.Containers[0]
			if len(got.Env) != 1 || got.Env[0].Name != ldPreloadEnv || got.Env[0].Value != tc.wantValue {
				t.Fatalf("preload environment = %#v, want %q", got.Env, tc.wantValue)
			}
			if !reflect.DeepEqual(got.EnvFrom, before.Spec.Containers[0].EnvFrom) {
				t.Fatal("preload configuration changed envFrom sources")
			}
		})
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
