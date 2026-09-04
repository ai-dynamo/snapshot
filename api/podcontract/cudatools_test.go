// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testAgentImage = "registry.example/snapshot-agent@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testDelivery() CUDAToolsDelivery {
	return CUDAToolsDelivery{AgentImage: testAgentImage}
}

func gpuContainer(name string, gpus int, command ...string) corev1.Container {
	c := corev1.Container{Name: name, Command: command}
	if gpus > 0 {
		c.Resources.Limits = corev1.ResourceList{GPUResourceName: *resource.NewQuantity(int64(gpus), resource.DecimalSI)}
	}
	return c
}

func template(containers ...corev1.Container) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{}, Spec: corev1.PodSpec{Containers: containers}}
}

func TestContainerGPUs(t *testing.T) {
	spec := &corev1.PodSpec{ResourceClaims: []corev1.PodResourceClaim{{Name: "gpus"}, {Name: "nics"}}}
	claimed := corev1.Container{Name: "w", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{GPUResourceName: resource.MustParse("2")},
		Claims:   []corev1.ResourceClaim{{Name: "gpus"}, {Name: "nics"}},
	}}
	counter := func(claim corev1.PodResourceClaim) (int, bool, error) {
		switch claim.Name {
		case "gpus":
			return 4, true, nil
		case "nics":
			return 0, false, nil
		}
		return 0, false, errors.New("unexpected claim")
	}
	gpus, undetermined, err := ContainerGPUs(spec, &claimed, counter)
	if err != nil || gpus != 6 || !undetermined {
		t.Fatalf("ContainerGPUs() = %d, %v, %v; want 6 (request + claim), undetermined", gpus, undetermined, err)
	}
	// Without a resolver every claim is unknown.
	if _, undetermined, err := ContainerGPUs(spec, &claimed, nil); err != nil || !undetermined {
		t.Fatalf("nil resolver: undetermined = %v, err = %v", undetermined, err)
	}
	// A limit wins over a request; no GPUs at all is zero.
	limited := gpuContainer("l", 1)
	limited.Resources.Requests = corev1.ResourceList{GPUResourceName: resource.MustParse("8")}
	if gpus, _, _ := ContainerGPUs(spec, &limited, nil); gpus != 1 {
		t.Fatalf("limit should win, got %d", gpus)
	}
	cpuOnly := corev1.Container{Name: "cpu"}
	if gpus, undetermined, err := ContainerGPUs(spec, &cpuOnly, nil); gpus != 0 || undetermined || err != nil {
		t.Fatalf("cpu-only container: %d %v %v", gpus, undetermined, err)
	}
	// A claim the pod does not declare is an error.
	dangling := corev1.Container{Name: "d"}
	dangling.Resources.Claims = []corev1.ResourceClaim{{Name: "missing"}}
	if _, _, err := ContainerGPUs(spec, &dangling, nil); err == nil {
		t.Fatal("undeclared claim was accepted")
	}
}

func TestShapeCUDALaunchJobWrapsOnlyMultiGPUTargets(t *testing.T) {
	tpl := template(
		gpuContainer("tp", 2, "python3", "-m", "worker"),
		gpuContainer("single", 1, "/usr/bin/worker"),
		gpuContainer("helper", 0, "sleep", "infinity"),
	)
	tpl.Spec.Containers[0].Args = []string{"--rank", "0"}

	// Shaping twice must be a no-op the second time.
	var wrapped []string
	for range 2 {
		var err error
		wrapped, err = ShapeCUDALaunchJob(tpl, []string{"tp", "single", "helper"}, testDelivery(), nil)
		if err != nil {
			t.Fatalf("ShapeCUDALaunchJob() failed: %v", err)
		}
	}
	if !reflect.DeepEqual(wrapped, []string{"tp"}) {
		t.Fatalf("wrapped = %v, want [tp]", wrapped)
	}
	if len(tpl.Spec.Volumes) != 1 || tpl.Spec.Volumes[0].Name != CUDAToolsVolumeName ||
		tpl.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("volumes = %#v", tpl.Spec.Volumes)
	}
	wantInit := expectedCUDAToolsInitContainer(testDelivery())
	if len(tpl.Spec.InitContainers) != 1 || !reflect.DeepEqual(tpl.Spec.InitContainers[0], wantInit) {
		t.Fatalf("init containers = %#v, want %#v", tpl.Spec.InitContainers, wantInit)
	}
	if wantInit.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("digest reference should default to IfNotPresent, got %s", wantInit.ImagePullPolicy)
	}
	tp := findContainer(&tpl.Spec, "tp")
	if !CUDAToolsMounted(tp) || !tp.VolumeMounts[0].ReadOnly {
		t.Fatalf("tp mounts = %#v", tp.VolumeMounts)
	}
	assertLaunchJob(t, tp, []string{"python3", "-m", "worker", "--rank", "0"})
	for _, name := range []string{"single", "helper"} {
		c := findContainer(&tpl.Spec, name)
		if len(c.VolumeMounts) != 0 || c.Command[0] == CUDACheckpointPath {
			t.Fatalf("%s must be untouched: %#v %#v", name, c.VolumeMounts, c.Command)
		}
	}
	if err := VerifyCUDALaunchJob(&tpl.Spec, []string{"tp"}); err != nil {
		t.Fatalf("VerifyCUDALaunchJob() on a shaped template failed: %v", err)
	}
	if !CUDAToolsDelivered(&tpl.Spec, "tp") || CUDAToolsDelivered(&tpl.Spec, "single") {
		t.Fatal("CUDAToolsDelivered() disagrees with the shape")
	}

	// A template with no multi-GPU target is not touched at all.
	plain := template(gpuContainer("single", 1, "/usr/bin/worker"))
	wrapped, err := ShapeCUDALaunchJob(plain, []string{"single"}, CUDAToolsDelivery{}, nil)
	if err != nil || wrapped != nil || len(plain.Spec.Volumes) != 0 || len(plain.Spec.InitContainers) != 0 {
		t.Fatalf("single-GPU template was touched: wrapped=%v err=%v spec=%#v", wrapped, err, plain.Spec)
	}
}

func TestShapeCUDALaunchJobTreatsUnknownClaimsAsMultiGPU(t *testing.T) {
	tpl := template(corev1.Container{
		Name: "dra", Command: []string{"worker"},
		Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpus"}}},
	})
	tpl.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpus"}}
	one := func(corev1.PodResourceClaim) (int, bool, error) { return 1, true, nil }
	wrapped, err := ShapeCUDALaunchJob(tpl.DeepCopy(), []string{"dra"}, testDelivery(), one)
	if err != nil || wrapped != nil {
		t.Fatalf("a one-device claim must not wrap: %v %v", wrapped, err)
	}
	unknown := func(corev1.PodResourceClaim) (int, bool, error) { return 0, false, nil }
	wrapped, err = ShapeCUDALaunchJob(tpl.DeepCopy(), []string{"dra"}, testDelivery(), unknown)
	if err != nil || !reflect.DeepEqual(wrapped, []string{"dra"}) {
		t.Fatalf("an unknown claim must wrap: %v %v", wrapped, err)
	}
	failing := func(corev1.PodResourceClaim) (int, bool, error) { return 0, false, errors.New("api down") }
	if _, err := ShapeCUDALaunchJob(tpl.DeepCopy(), []string{"dra"}, testDelivery(), failing); err == nil {
		t.Fatal("resolver errors must propagate")
	}
}

func TestShapeCUDALaunchJobDeliveryOptions(t *testing.T) {
	tpl := template(gpuContainer("w", 2, "worker"))
	tpl.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "existing"}}
	delivery := CUDAToolsDelivery{
		AgentImage:       "registry.example/snapshot-agent:v1.2.3",
		PullPolicy:       corev1.PullNever,
		ImagePullSecrets: []string{"registry-creds", " ", "existing", "registry-creds"},
	}
	if _, err := ShapeCUDALaunchJob(tpl, []string{"w"}, delivery, nil); err != nil {
		t.Fatalf("ShapeCUDALaunchJob() failed: %v", err)
	}
	if got := tpl.Spec.InitContainers[0].ImagePullPolicy; got != corev1.PullNever {
		t.Fatalf("pull policy = %s, want Never", got)
	}
	var names []string
	for _, ref := range tpl.Spec.ImagePullSecrets {
		names = append(names, ref.Name)
	}
	if want := []string{"existing", "registry-creds"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("imagePullSecrets = %v, want %v", names, want)
	}
	tagged := CUDAToolsDelivery{AgentImage: "registry.example/agent:v1"}
	if got := expectedCUDAToolsInitContainer(tagged).ImagePullPolicy; got != corev1.PullAlways {
		t.Fatalf("tag reference should default to Always, got %s", got)
	}

	// Reshaping a live object the API server has defaulted is fine; a
	// different image in the init container is not.
	tpl.Spec.InitContainers[0].TerminationMessagePath = "/dev/termination-log"
	if _, err := ShapeCUDALaunchJob(tpl, []string{"w"}, delivery, nil); err != nil {
		t.Fatalf("reshaping a defaulted template failed: %v", err)
	}
	tpl.Spec.InitContainers[0].Image = "someone-else/image:latest"
	if _, err := ShapeCUDALaunchJob(tpl, []string{"w"}, delivery, nil); err == nil {
		t.Fatal("init container with a different image was accepted")
	}
}

func TestShapeCUDALaunchJobRejectsInvalidContract(t *testing.T) {
	worker := gpuContainer("worker", 2, "worker")
	tests := map[string]struct {
		template *corev1.PodTemplateSpec
		targets  []string
		delivery CUDAToolsDelivery
	}{
		"implicit image entrypoint": {template(gpuContainer("worker", 2)), []string{"worker"}, testDelivery()},
		"missing agent image":       {template(worker), []string{"worker"}, CUDAToolsDelivery{}},
		"malformed agent image": {template(worker), []string{"worker"}, CUDAToolsDelivery{
			AgentImage: "registry.example/not valid@sha256:" + strings.Repeat("0", 64),
		}},
		"unknown target": {template(worker), []string{"other"}, testDelivery()},
		"no targets":     {template(worker), nil, testDelivery()},
		"conflicting mount options": {
			template(corev1.Container{
				Name: "worker", Command: []string{"worker"},
				Resources:    worker.Resources,
				VolumeMounts: []corev1.VolumeMount{{Name: CUDAToolsVolumeName, MountPath: CUDAToolsMountPath, SubPath: "x"}},
			}), []string{"worker"}, testDelivery(),
		},
		"volume is not an emptyDir": {
			func() *corev1.PodTemplateSpec {
				tpl := template(worker)
				tpl.Spec.Volumes = []corev1.Volume{{
					Name:         CUDAToolsVolumeName,
					VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp"}},
				}}
				return tpl
			}(), []string{"worker"}, testDelivery(),
		},
		"foreign cuda-checkpoint wrapper": {
			template(corev1.Container{
				Name: "worker", Resources: worker.Resources,
				Command: []string{CUDACheckpointPath}, Args: []string{"--something-else"},
			}), []string{"worker"}, testDelivery(),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			before := tc.template.DeepCopy()
			if _, err := ShapeCUDALaunchJob(tc.template, tc.targets, tc.delivery, nil); err == nil {
				t.Fatal("ShapeCUDALaunchJob() succeeded, want error")
			}
			if !reflect.DeepEqual(tc.template, before) {
				t.Fatalf("failed shape mutated template:\ngot:  %#v\nwant: %#v", tc.template, before)
			}
		})
	}
}

func TestVerifyCUDALaunchJobRejectsUnshapedSpecs(t *testing.T) {
	shaped := template(gpuContainer("worker", 2, "worker"))
	if _, err := ShapeCUDALaunchJob(shaped, []string{"worker"}, testDelivery(), nil); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*corev1.PodSpec){
		"no volume":         func(s *corev1.PodSpec) { s.Volumes = nil },
		"no init container": func(s *corev1.PodSpec) { s.InitContainers = nil },
		"no mount":          func(s *corev1.PodSpec) { s.Containers[0].VolumeMounts = nil },
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
			if err := VerifyCUDALaunchJob(spec, []string{"worker"}); err == nil {
				t.Fatal("VerifyCUDALaunchJob() accepted an unshaped spec")
			}
		})
	}
}

func TestValidateImageReference(t *testing.T) {
	valid := []string{
		"ghcr.io/ai-dynamo/snapshot/agent:v0.1.0",
		"ghcr.io/ai-dynamo/snapshot/agent",
		"localhost:5000/agent:dev",
		"agent",
		testAgentImage,
		"registry.example/agent:v1@sha256:" + strings.Repeat("a", 64),
	}
	for _, image := range valid {
		if err := ValidateImageReference(image); err != nil {
			t.Errorf("ValidateImageReference(%q) = %v, want nil", image, err)
		}
	}
	invalid := []string{
		"",
		" ghcr.io/agent:v1",
		"registry.example/not valid@sha256:" + strings.Repeat("0", 64),
		"registry.example/Agent:v1",
		"registry.example/agent@sha256:short",
		"registry.example/agent:tag with space",
	}
	for _, image := range invalid {
		if err := ValidateImageReference(image); err == nil {
			t.Errorf("ValidateImageReference(%q) = nil, want error", image)
		}
	}
}

func assertLaunchJob(t *testing.T, container *corev1.Container, original []string) {
	t.Helper()
	if !reflect.DeepEqual(container.Command, []string{CUDACheckpointPath}) {
		t.Fatalf("%s command = %#v", container.Name, container.Command)
	}
	if !isCUDACheckpointLaunchJob(container.Args) {
		t.Fatalf("%s is not wrapped with cuda-checkpoint: %#v", container.Name, container.Args)
	}
	if got := container.Args[6:]; !reflect.DeepEqual(got, original) {
		t.Fatalf("%s original command = %#v, want %#v", container.Name, got, original)
	}
}
