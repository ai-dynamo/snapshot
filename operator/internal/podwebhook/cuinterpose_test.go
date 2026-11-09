// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podwebhook

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	"github.com/ai-dynamo/snapshot/operator/internal/protocol"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

func testInstaller() operatortypes.CuInterposeContainerConfiguration {
	return operatortypes.CuInterposeContainerConfiguration{Image: "registry.example/snapshot-agent:v1", PullPolicy: corev1.PullIfNotPresent}
}

func gpuResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Limits: corev1.ResourceList{gpuResource: resource.MustParse("8")}}
}

// optedInPod has one GPU container with an explicit command beside a sidecar.
func optedInPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "worker",
			Annotations: map[string]string{podcontract.CuInterposeAnnotation: "enabled"},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "workload",
			ImagePullSecrets:   []corev1.LocalObjectReference{{Name: "pull"}},
			Containers: []corev1.Container{
				{
					Name: "main", Image: "registry.example/vllm:v1",
					Command: []string{"python3", "app.py"}, Args: []string{"--tp", "8"},
					Env:       []corev1.EnvVar{{Name: "LD_PRELOAD", Value: "/opt/other.so"}},
					Resources: gpuResources(),
				},
				{Name: "metrics", Image: "registry.example/exporter:v1"},
			},
		},
	}
}

func newInjector() *CuInterposeInjector {
	return NewCuInterposeInjector(clientgoscheme.Scheme, testInstaller())
}

func TestShapeWrapsGPUContainersOnly(t *testing.T) {
	pod := optedInPod()
	before := pod.DeepCopy()
	outcome, err := newInjector().shape(context.Background(), pod)
	require.NoError(t, err)
	assert.True(t, outcome.changed)
	assert.Equal(t, []string{"main"}, outcome.targets)

	main := pod.Spec.Containers[0]
	assert.Equal(t, append(podcontract.CuInterposeLauncherPrefix(), "python3", "app.py"), main.Command)
	require.NoError(t, podcontract.ValidateCuInterposeLauncher(&main))
	assert.Equal(t, before.Spec.Containers[0].Args, main.Args)
	assert.Equal(t, before.Spec.Containers[0].Env, main.Env)
	assert.Equal(t, before.Spec.Containers[1], pod.Spec.Containers[1], "the sidecar is untouched")
	require.Len(t, pod.Spec.InitContainers, 1)
	assert.Equal(t, testInstaller().Image, pod.Spec.InitContainers[0].Image)
	require.Len(t, pod.Spec.Volumes, 1)
	assert.Equal(t, before.ObjectMeta, pod.ObjectMeta)
}

func TestShapeLeavesPodsAlone(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(*corev1.Pod)
		wantReason  string
		wantWarning bool
	}{
		{"no annotation", func(p *corev1.Pod) { p.Annotations = nil }, "does not opt into", false},
		{"disabled", func(p *corev1.Pod) { p.Annotations[podcontract.CuInterposeAnnotation] = " disabled " }, "does not opt into", false},
		{"installer present", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "snapshot-cuda-install"}}
		}, "already carries", false},
		{"volume present", func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "snapshot-cuda"}}
		}, "already carries", false},
		{"launcher already wraps the command", func(p *corev1.Pod) {
			p.Spec.Containers[0].Command = append(podcontract.CuInterposeLauncherPrefix(), "python3")
		}, "already starts under the launcher", false},
		{"no GPU container", func(p *corev1.Pod) { p.Spec.Containers[0].Resources = corev1.ResourceRequirements{} }, "no container requests a GPU", true},
		{"zero GPUs", func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits[gpuResource] = resource.MustParse("0")
		}, "no container requests a GPU", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := optedInPod()
			tc.change(pod)
			before := pod.DeepCopy()
			outcome, err := newInjector().shape(context.Background(), pod)
			require.NoError(t, err)
			assert.False(t, outcome.changed)
			assert.Contains(t, outcome.reason, tc.wantReason)
			assert.Equal(t, tc.wantWarning, outcome.warning != "")
			assert.Equal(t, before, pod)
		})
	}
}

func TestShapeCountsMIGAndRequestsAsGPU(t *testing.T) {
	for name, resources := range map[string]corev1.ResourceRequirements{
		"mig limit":   {Limits: corev1.ResourceList{"nvidia.com/mig-1g.5gb": resource.MustParse("1")}},
		"gpu request": {Requests: corev1.ResourceList{gpuResource: resource.MustParse("1")}},
	} {
		t.Run(name, func(t *testing.T) {
			pod := optedInPod()
			pod.Spec.Containers[0].Resources = resources
			outcome, err := newInjector().shape(context.Background(), pod)
			require.NoError(t, err)
			assert.Equal(t, []string{"main"}, outcome.targets)
		})
	}
}

func TestShapeDeniesWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod)
		want   string
	}{
		{
			name:   "invalid annotation",
			change: func(p *corev1.Pod) { p.Annotations[podcontract.CuInterposeAnnotation] = "yes" },
			want:   "expected enabled or disabled",
		},
		{
			name:   "image-default entrypoint",
			change: func(p *corev1.Pod) { p.Spec.Containers[0].Command = nil },
			want:   `container "main": cuinterpose requires container.command`,
		},
		{
			name: "delivery path in use",
			change: func(p *corev1.Pod) {
				p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: podcontract.CuInterposeMountPath}}
			},
			want: "conflicts with cuinterpose delivery",
		},
		{
			name: "second GPU container without command",
			change: func(p *corev1.Pod) {
				p.Spec.Containers[1].Resources = gpuResources()
			},
			want: `container "metrics": cuinterpose requires container.command`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := optedInPod()
			tc.change(pod)
			before := pod.DeepCopy()
			_, err := newInjector().shape(context.Background(), pod)
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, before, pod)
		})
	}
}

func admissionRequest(t *testing.T, operation admissionv1.Operation, pod *corev1.Pod) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	require.NoError(t, err)
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: operation,
		Namespace: "team",
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

func patchPaths(response admission.Response) []string {
	paths := make([]string, 0, len(response.Patches))
	for _, patch := range response.Patches {
		paths = append(paths, patch.Path)
	}
	return paths
}

func TestHandlePatchesOptedInPods(t *testing.T) {
	response := newInjector().Handle(context.Background(), admissionRequest(t, admissionv1.Create, optedInPod()))
	require.True(t, response.Allowed, response.Result)
	paths := patchPaths(response)
	assert.Contains(t, paths, "/spec/initContainers")
	assert.Contains(t, paths, "/spec/volumes")
	assert.Contains(t, paths, "/spec/containers/0/volumeMounts")
	assert.True(t, slices.ContainsFunc(paths, func(path string) bool {
		return strings.HasPrefix(path, "/spec/containers/0/command/")
	}), paths)
	for _, path := range paths {
		assert.NotContains(t, path, "/spec/containers/1", "the sidecar is untouched")
		assert.NotContains(t, path, "/metadata")
	}
}

func TestHandleKeepsFieldsTheOperatorDoesNotKnow(t *testing.T) {
	// A newer API server can send spec fields this operator's API types lack.
	// They must survive in the spec, in a target container, and in the existing
	// volumes and init containers the shaping appends to.
	raw := []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"worker","namespace":"team",` +
		`"annotations":{"nvidia.com/snapshot-multi-gpu":"enabled"}},"spec":{"futureSpecField":{"on":true},` +
		`"initContainers":[{"name":"setup","image":"registry.example/setup:v1","futureContainerField":"kept"}],` +
		`"volumes":[{"name":"data","emptyDir":{},"futureVolumeField":1}],` +
		`"containers":[{"name":"main","image":"registry.example/vllm:v1","command":["python3"],"args":["app.py"],` +
		`"futureContainerField":"kept","volumeMounts":[{"name":"data","mountPath":"/data","futureMountField":true}],` +
		`"resources":{"limits":{"nvidia.com/gpu":"1"}}}]}}`)
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create, Namespace: "team", Object: runtime.RawExtension{Raw: raw},
	}}
	response := newInjector().Handle(context.Background(), request)
	require.True(t, response.Allowed, response.Result)
	require.NotEmpty(t, response.Patches)
	for _, patch := range response.Patches {
		assert.NotEqual(t, "remove", patch.Operation, patch.Path)
		assert.NotContains(t, patch.Path, "future", "unknown fields are never touched")
	}
	paths := patchPaths(response)
	assert.Contains(t, paths, "/spec/volumes/1")
	assert.Contains(t, paths, "/spec/initContainers/1")
	assert.Contains(t, paths, "/spec/containers/0/volumeMounts/1")
	assert.True(t, slices.ContainsFunc(paths, func(path string) bool {
		return strings.HasPrefix(path, "/spec/containers/0/command")
	}), paths)
	for _, path := range paths {
		assert.NotContains(t, path, "/spec/containers/0/args")
	}
}

func TestApplyShapingAppendsOnly(t *testing.T) {
	raw := []byte(`{"spec":{"containers":[{"name":"main","command":["python3"],"unknown":1}]}}`)
	original := &corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Command: []string{"python3"}}}}
	shaped := original.DeepCopy()
	require.NoError(t, protocol.ShapeCuInterposeCapture(shaped, []string{"main"}, testInstaller()))

	out, err := applyShaping(raw, original, shaped, []string{"main"})
	require.NoError(t, err)
	var decoded struct {
		Spec struct {
			Volumes        []map[string]any `json:"volumes"`
			InitContainers []map[string]any `json:"initContainers"`
			Containers     []map[string]any `json:"containers"`
		} `json:"spec"`
	}
	require.NoError(t, json.Unmarshal(out, &decoded))
	require.Len(t, decoded.Spec.Volumes, 1)
	require.Len(t, decoded.Spec.InitContainers, 1)
	require.Len(t, decoded.Spec.Containers, 1)
	main := decoded.Spec.Containers[0]
	assert.EqualValues(t, 1, main["unknown"])
	assert.Len(t, main["command"], len(podcontract.CuInterposeLauncherPrefix())+1)
	assert.Len(t, main["volumeMounts"], 1)
	_, hasArgs := main["args"]
	assert.False(t, hasArgs, "args are never written when the Pod set none")
}

func TestHandlePatchesOnlyTheSpecOfServerShapedObjects(t *testing.T) {
	// The API server serializes fields the Go struct leaves out, such as a null
	// creationTimestamp and an empty status. The patch must not touch them.
	raw := []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"worker","namespace":"team","creationTimestamp":null,` +
		`"annotations":{"nvidia.com/snapshot-multi-gpu":"enabled"}},"spec":{"containers":[{"name":"main","image":"registry.example/vllm:v1",` +
		`"command":["python3"],"resources":{"limits":{"nvidia.com/gpu":"1"}}}]},"status":{}}`)
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create, Namespace: "team", Object: runtime.RawExtension{Raw: raw},
	}}
	response := newInjector().Handle(context.Background(), request)
	require.True(t, response.Allowed, response.Result)
	require.NotEmpty(t, response.Patches)
	for _, path := range patchPaths(response) {
		assert.True(t, strings.HasPrefix(path, "/spec/"), path)
	}
}

func TestHandleAllowsUnchangedPods(t *testing.T) {
	pod := optedInPod()
	pod.Annotations = nil
	response := newInjector().Handle(context.Background(), admissionRequest(t, admissionv1.Create, pod))
	assert.True(t, response.Allowed)
	assert.Empty(t, response.Patches)
	assert.Empty(t, response.Warnings)

	noGPU := optedInPod()
	noGPU.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	response = newInjector().Handle(context.Background(), admissionRequest(t, admissionv1.Create, noGPU))
	assert.True(t, response.Allowed)
	assert.Empty(t, response.Patches)
	require.Len(t, response.Warnings, 1)
	assert.Contains(t, response.Warnings[0], "no container requests a GPU")
}

func TestHandleIgnoresOtherOperations(t *testing.T) {
	response := newInjector().Handle(context.Background(), admissionRequest(t, admissionv1.Update, optedInPod()))
	assert.True(t, response.Allowed)
	assert.Empty(t, response.Patches)
}

func TestHandleDeniesUnpreparablePods(t *testing.T) {
	pod := optedInPod()
	pod.Annotations[podcontract.CuInterposeAnnotation] = "yes"
	response := newInjector().Handle(context.Background(), admissionRequest(t, admissionv1.Create, pod))
	assert.False(t, response.Allowed)
	assert.Contains(t, response.Result.Message, "expected enabled or disabled")
}

func TestHandleRejectsMalformedObjects(t *testing.T) {
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: []byte("{")},
	}}
	response := newInjector().Handle(context.Background(), request)
	assert.False(t, response.Allowed)
	assert.EqualValues(t, 400, response.Result.Code)
}
