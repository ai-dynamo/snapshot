// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func cuInterposeTemplate() *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{podcontract.CuInterposeAnnotation: "true"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "worker", Command: []string{"python3", "-m", "worker"}, Args: []string{"--rank", "0"}},
			{Name: "helper"},
		}},
	}
}

func testCuInterposeDelivery() CuInterposeDelivery {
	return CuInterposeDelivery{AgentImage: "registry.example/snapshot-agent:v1", PullPolicy: corev1.PullNever}
}

func TestShapeCuInterposeCapture(t *testing.T) {
	template := cuInterposeTemplate()
	template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "LD_PRELOAD", Value: "/opt/first.so:/opt/second.so"},
		{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "workload-env"}, Key: "value",
		}}},
	}
	template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}}},
		{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "secret"}}},
	}
	template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "existing"}}
	before := template.DeepCopy()
	require.NoError(t, shapeCuInterposeCapture(template, "worker", testCuInterposeDelivery()))

	worker := template.Spec.Containers[0]
	assert.Equal(t, append([]string{podcontract.CuInterposeLauncherPath}, before.Spec.Containers[0].Command...), worker.Command)
	assert.Equal(t, before.Spec.Containers[0].Args, worker.Args)
	assert.Equal(t, before.Spec.Containers[0].Env, worker.Env)
	assert.Equal(t, before.Spec.Containers[0].EnvFrom, worker.EnvFrom)
	assert.Equal(t, before.Spec.Containers[1], template.Spec.Containers[1])
	assert.Equal(t, before.Spec.ImagePullSecrets, template.Spec.ImagePullSecrets)
	assert.Equal(t, []corev1.VolumeMount{{Name: cuInterposeVolumeName, MountPath: podcontract.CuInterposeMountPath, ReadOnly: true}}, worker.VolumeMounts)
	require.Len(t, template.Spec.Volumes, 1)
	assert.Equal(t, cuInterposeVolumeName, template.Spec.Volumes[0].Name)
	assert.NotNil(t, template.Spec.Volumes[0].EmptyDir)
	require.Len(t, template.Spec.InitContainers, 1)
	installer := template.Spec.InitContainers[0]
	assert.Equal(t, testCuInterposeDelivery().AgentImage, installer.Image)
	assert.Equal(t, corev1.PullNever, installer.ImagePullPolicy)
	assert.Equal(t, []string{"/bin/cp"}, installer.Command)
	assert.Equal(t, []string{
		"--preserve=mode", "--",
		"/usr/local/lib/snapshot/libcuinterpose.so", "/usr/local/lib/snapshot/libcuinterpose_core.so",
		"/usr/local/bin/cuinterpose-launch", podcontract.CuInterposeMountPath + "/",
	}, installer.Args)
	assert.Equal(t, corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi"),
	}, installer.Resources.Requests)
	assert.Equal(t, installer.Resources.Requests, installer.Resources.Limits)

	// Delivery is apply-once: even an identical prior injection is a collision.
	injected := template.DeepCopy()
	require.ErrorContains(t, shapeCuInterposeCapture(template, "worker", testCuInterposeDelivery()), "reserved")
	assert.Equal(t, injected, template)
}

func TestShapeCuInterposeCapturePreservesPreloadSources(t *testing.T) {
	for _, env := range [][]corev1.EnvVar{
		nil,
		{{Name: "LD_PRELOAD", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "config"}, Key: "preload",
		}}}},
		{{Name: "LD_PRELOAD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "secret"}, Key: "preload",
		}}}},
	} {
		template := cuInterposeTemplate()
		template.Spec.Containers[0].Env = env
		template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{
			ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}},
		}}
		before := template.DeepCopy()
		require.NoError(t, shapeCuInterposeCapture(template, "worker", testCuInterposeDelivery()))
		assert.Equal(t, before.Spec.Containers[0].Env, template.Spec.Containers[0].Env)
		assert.Equal(t, before.Spec.Containers[0].EnvFrom, template.Spec.Containers[0].EnvFrom)
	}
}

func TestShapeCuInterposeCaptureRejectsWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.PodTemplateSpec, *CuInterposeDelivery)
		want   string
	}{
		{"missing target", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) { p.Spec.Containers[0].Name = "other" }, "not found"},
		{"missing command", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) { p.Spec.Containers[0].Command = nil }, "requires container.command"},
		{"empty command", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) { p.Spec.Containers[0].Command = []string{""} }, "requires container.command"},
		{"reserved volume", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) {
			p.Spec.Volumes = []corev1.Volume{{Name: cuInterposeVolumeName}}
		}, "volume name"},
		{"reserved init name", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) {
			p.Spec.InitContainers = []corev1.Container{{Name: cuInterposeInitContainerName}}
		}, "container name"},
		{"reserved regular name", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) {
			p.Spec.Containers[1].Name = cuInterposeInitContainerName
		}, "container name"},
		{"mount path", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) {
			p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "other", MountPath: podcontract.CuInterposeMountPath + "/"}}
		}, "conflicts"},
		{"nested mount path", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) {
			p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "other", MountPath: podcontract.CuInterposeLibraryPath}}
		}, "conflicts"},
		{"mount name", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) {
			p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: cuInterposeVolumeName, MountPath: "/other"}}
		}, "conflicts"},
		{"invalid annotation", func(p *corev1.PodTemplateSpec, _ *CuInterposeDelivery) {
			p.Annotations[podcontract.CuInterposeAnnotation] = "enabled"
		}, "invalid boolean"},
		{"missing image", func(_ *corev1.PodTemplateSpec, d *CuInterposeDelivery) { d.AgentImage = "" }, "--agent-image"},
		{"unsupported policy", func(_ *corev1.PodTemplateSpec, d *CuInterposeDelivery) { d.PullPolicy = "sometimes" }, "pull policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template, delivery := cuInterposeTemplate(), testCuInterposeDelivery()
			tc.change(template, &delivery)
			before := template.DeepCopy()
			require.ErrorContains(t, shapeCuInterposeCapture(template, "worker", delivery), tc.want)
			assert.Equal(t, before, template)
		})
	}
}

func TestShapeCuInterposeCaptureDisabled(t *testing.T) {
	for _, annotations := range []map[string]string{nil, {podcontract.CuInterposeAnnotation: "false"}} {
		template := cuInterposeTemplate()
		template.Annotations = annotations
		before := template.DeepCopy()
		require.NoError(t, shapeCuInterposeCapture(template, "absent", CuInterposeDelivery{}))
		assert.Equal(t, before, template)
	}
}

func TestCuInterposeInstallerSecurityContext(t *testing.T) {
	for _, tc := range []struct {
		name     string
		security *corev1.PodSecurityContext
		wantUID  int64
	}{
		{name: "default", wantUID: 65532},
		{name: "nonroot without UID", security: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)}, wantUID: 65532},
		{name: "nonroot UID", security: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)}, wantUID: 1000},
		{name: "root workload", security: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](0)}, wantUID: 65532},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := cuInterposeTemplate()
			template.Spec.SecurityContext = tc.security
			template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To[int64](2000)}
			before := template.DeepCopy()
			require.NoError(t, shapeCuInterposeCapture(template, "worker", testCuInterposeDelivery()))
			assert.Equal(t, &corev1.SecurityContext{
				RunAsUser: ptr.To(tc.wantUID), RunAsNonRoot: ptr.To(true),
				AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
				Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			}, template.Spec.InitContainers[0].SecurityContext)
			assert.Equal(t, before.Spec.SecurityContext, template.Spec.SecurityContext)
			assert.Equal(t, before.Spec.Containers[0].SecurityContext, template.Spec.Containers[0].SecurityContext)
		})
	}
}

func TestCuInterposeDeliveryPullPolicy(t *testing.T) {
	for _, policy := range []corev1.PullPolicy{"", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever} {
		require.NoError(t, (CuInterposeDelivery{PullPolicy: policy}).ValidatePullPolicy())
	}
	require.ErrorContains(t, (CuInterposeDelivery{PullPolicy: "invalid"}).ValidatePullPolicy(), "pull policy")
}
