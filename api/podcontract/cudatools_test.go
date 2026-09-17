// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const testAgentImage = "registry.example/snapshot-agent@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testDelivery() CUDAToolsDelivery {
	return CUDAToolsDelivery{AgentImage: testAgentImage}
}

func gpuContainer(name string, gpus int, command ...string) corev1.Container {
	c := corev1.Container{Name: name, Command: command}
	if gpus > 0 {
		c.Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(int64(gpus), resource.DecimalSI)}
	}
	return c
}

func template(containers ...corev1.Container) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: containers}}
}

func TestCUDAToolsDeliveryPreservesCommands(t *testing.T) {
	tpl := template(gpuContainer("multi", 8, "python", "worker.py"), gpuContainer("implicit", 1))
	tpl.Spec.Containers[0].Args = []string{"--rank", "0"}
	tpl.Spec.Containers[1].Resources.Claims = []corev1.ResourceClaim{{Name: "gpus"}}
	tpl.Spec.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpus"}}
	before := tpl.DeepCopy()
	for range 2 {
		if err := ShapeCUDATools(tpl, []string{"multi", "implicit"}, testDelivery()); err != nil {
			t.Fatal(err)
		}
	}
	if len(tpl.Spec.Volumes) != 1 || len(tpl.Spec.InitContainers) != 1 {
		t.Fatalf("duplicate delivery resources: %#v", tpl.Spec)
	}
	for i := range tpl.Spec.Containers {
		got, want := tpl.Spec.Containers[i], before.Spec.Containers[i]
		if !reflect.DeepEqual(got.Command, want.Command) || !reflect.DeepEqual(got.Args, want.Args) {
			t.Fatalf("command rewritten: %#v", got)
		}
		if !CUDAToolsDelivered(&tpl.Spec, got.Name) || !got.VolumeMounts[0].ReadOnly {
			t.Fatalf("missing read-only delivery: %#v", got)
		}
	}
	if err := VerifyCUDATools(&tpl.Spec, []string{"multi", "implicit"}); err != nil {
		t.Fatal(err)
	}
	want := expectedCUDAToolsInitContainer(testDelivery())
	if !reflect.DeepEqual(tpl.Spec.InitContainers[0], want) {
		t.Fatalf("installer mismatch: %#v", tpl.Spec.InitContainers)
	}
	for _, missing := range []string{cudaToolsImageLibrary, cudaToolsImageCoreLibrary} {
		partial := want.DeepCopy()
		partial.Args = nil
		for _, arg := range want.Args {
			if arg != missing {
				partial.Args = append(partial.Args, arg)
			}
		}
		if initContainerMatches(partial, &want) {
			t.Fatal("accepted installer missing a library")
		}
	}
}

func TestCUDAToolsDeliveryOptions(t *testing.T) {
	tpl := template(gpuContainer("w", 2, "worker"))
	tpl.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "existing"}}
	delivery := CUDAToolsDelivery{
		AgentImage: "registry.example/agent:v1", PullPolicy: corev1.PullNever,
		ImagePullSecrets: []string{"registry-creds", " ", "existing", "registry-creds"},
	}
	if err := ShapeCUDATools(tpl, []string{"w"}, delivery); err != nil {
		t.Fatal(err)
	}
	if tpl.Spec.InitContainers[0].ImagePullPolicy != corev1.PullNever {
		t.Fatal("pull policy not preserved")
	}
	want := []corev1.LocalObjectReference{{Name: "existing"}, {Name: "registry-creds"}}
	if !reflect.DeepEqual(tpl.Spec.ImagePullSecrets, want) {
		t.Fatalf("pull secrets = %#v", tpl.Spec.ImagePullSecrets)
	}
	if expectedCUDAToolsInitContainer(testDelivery()).ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatal("digest reference should default to IfNotPresent")
	}
	delivery.PullPolicy = ""
	if expectedCUDAToolsInitContainer(delivery).ImagePullPolicy != corev1.PullAlways {
		t.Fatal("tag reference should default to Always")
	}
}

func TestCUDAToolsRejectsConflictingContracts(t *testing.T) {
	for _, mutation := range []func(*corev1.PodTemplateSpec){
		func(tpl *corev1.PodTemplateSpec) { tpl.Spec.Volumes[0].EmptyDir = nil },
		func(tpl *corev1.PodTemplateSpec) { tpl.Spec.InitContainers[0].Image = "foreign/image" },
		func(tpl *corev1.PodTemplateSpec) { tpl.Spec.Containers[0].VolumeMounts[0].SubPath = "other" },
	} {
		tpl := template(gpuContainer("w", 2, "worker"))
		if err := ShapeCUDATools(tpl, []string{"w"}, testDelivery()); err != nil {
			t.Fatal(err)
		}
		mutation(tpl)
		before := tpl.DeepCopy()
		if err := ShapeCUDATools(tpl, []string{"w"}, testDelivery()); err == nil {
			t.Fatal("conflicting contract accepted")
		}
		if !reflect.DeepEqual(tpl, before) {
			t.Fatal("failed shaping mutated template")
		}
	}
	for _, targets := range [][]string{nil, {"missing"}} {
		if err := ShapeCUDATools(template(gpuContainer("w", 1)), targets, testDelivery()); err == nil {
			t.Fatal("invalid targets accepted")
		}
	}
}

func TestValidateImageReference(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/ai-dynamo/snapshot/agent:v0.1.0", "localhost:5000/agent:dev", "agent", testAgentImage,
	} {
		if err := ValidateImageReference(image); err != nil {
			t.Errorf("valid image %q: %v", image, err)
		}
	}
	for _, image := range []string{
		"", " ghcr.io/agent:v1", "registry.example/Agent:v1", "agent@sha256:short",
		"agent:tag with space", "not valid@sha256:" + strings.Repeat("0", 64),
	} {
		if err := ValidateImageReference(image); err == nil {
			t.Errorf("invalid image accepted: %q", image)
		}
	}
}
