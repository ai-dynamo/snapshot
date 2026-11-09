// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package podwebhook serves the operator's mutating admission webhook for Pods.
//
// A Pod that opts into multi-GPU support with the nvidia.com/snapshot-multi-gpu
// annotation needs the cuInterpose libraries installed and its GPU containers
// started under the launcher before any CUDA call. SnapshotJob prepares its own
// source Pod; this webhook prepares every other opted-in Pod, whether created
// directly or through a Deployment, on clusters whose runtime offers no NRI.
//
// Every GPU container must set an explicit command: admission runs before any
// runtime resolves the image, so the launcher has nothing to wrap otherwise.
package podwebhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	"github.com/ai-dynamo/snapshot/operator/internal/protocol"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

// CuInterposePath is where the manager serves the Pod mutation.
const CuInterposePath = "/mutate-v1-pod"

// gpuResource is the device plugin resource a multi-GPU container requests.
const gpuResource corev1.ResourceName = "nvidia.com/gpu"

// migResourcePrefix starts the MIG device plugin resources (nvidia.com/mig-1g.5gb).
const migResourcePrefix = "nvidia.com/mig-"

// CuInterposeInjector shapes opted-in Pods on creation, the way SnapshotJob shapes
// its source Job template.
type CuInterposeInjector struct {
	Installer operatortypes.CuInterposeContainerConfiguration
	decoder   admission.Decoder
}

// NewCuInterposeInjector returns an injector decoding Pods with scheme.
func NewCuInterposeInjector(scheme *runtime.Scheme, installer operatortypes.CuInterposeContainerConfiguration) *CuInterposeInjector {
	return &CuInterposeInjector{Installer: installer, decoder: admission.NewDecoder(scheme)}
}

// Handle implements admission.Handler.
func (i *CuInterposeInjector) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("only Pod creation is shaped")
	}
	pod := &corev1.Pod{}
	if err := i.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("namespace", req.Namespace, "pod", pod.Name, "generateName", pod.GenerateName))
	outcome, err := i.shape(ctx, pod)
	if err != nil {
		return admission.Denied(err.Error())
	}
	if !outcome.changed {
		response := admission.Allowed(outcome.reason)
		if outcome.warning != "" {
			response = response.WithWarnings(outcome.warning)
		}
		return response
	}
	shaped, err := replaceSpec(req.Object.Raw, &pod.Spec)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	log.FromContext(ctx).Info("prepared Pod for cuinterpose", "containers", outcome.targets)
	return admission.PatchResponseFromRaw(req.Object.Raw, shaped)
}

// replaceSpec puts spec into the Pod as the API server serialized it, so the
// patch touches nothing but the spec: re-encoding the whole Go struct would also
// rewrite fields the struct renders differently, such as a null creationTimestamp.
func replaceSpec(raw []byte, spec *corev1.PodSpec) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("decode admitted Pod: %w", err)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("encode shaped Pod spec: %w", err)
	}
	object["spec"] = encoded
	return json.Marshal(object)
}

// shapeOutcome says what shape did to the Pod, and why when it did nothing.
type shapeOutcome struct {
	changed bool
	reason  string
	warning string
	targets []string
}

// shape prepares an opted-in Pod in place; it validates everything first, so a
// denied Pod is left as it was. The returned error is a denial: the Pod asked for
// multi-GPU support that it cannot receive as written, and admitting it unprepared
// would only move the failure to capture preflight.
func (i *CuInterposeInjector) shape(ctx context.Context, pod *corev1.Pod) (shapeOutcome, error) {
	enabled, err := podcontract.ParseCuInterposeAnnotation(pod.Annotations)
	if err != nil {
		return shapeOutcome{}, err
	}
	if !enabled {
		return shapeOutcome{reason: "Pod does not opt into cuinterpose"}, nil
	}
	if protocol.CuInterposePrepared(&pod.Spec) {
		return shapeOutcome{reason: "Pod already carries the cuinterpose delivery"}, nil
	}
	gpuContainers, targets := cuInterposeTargets(&pod.Spec)
	switch {
	case len(gpuContainers) == 0:
		return shapeOutcome{
			reason:  "no container requests a GPU",
			warning: fmt.Sprintf("%s is enabled but no container requests a GPU, so none was prepared for cuinterpose", podcontract.CuInterposeAnnotation),
		}, nil
	case len(targets) == 0:
		// The Pod brings the launcher its own way, as the workload contract allows.
		return shapeOutcome{reason: "every GPU container already starts under the launcher"}, nil
	}
	if err := protocol.ShapeCuInterposeCapture(&pod.Spec, targets, i.Installer); err != nil {
		return shapeOutcome{}, err
	}
	log.FromContext(ctx).V(1).Info("shaped Pod for cuinterpose", "containers", targets)
	return shapeOutcome{changed: true, targets: targets}, nil
}

// cuInterposeTargets names the containers that request a GPU, and among them the
// ones to start under the launcher: those that do not already do so.
func cuInterposeTargets(spec *corev1.PodSpec) (gpuContainers, targets []string) {
	for i := range spec.Containers {
		container := &spec.Containers[i]
		if !requestsGPU(container) {
			continue
		}
		gpuContainers = append(gpuContainers, container.Name)
		if !startsWithLauncher(container) {
			targets = append(targets, container.Name)
		}
	}
	return gpuContainers, targets
}

func requestsGPU(container *corev1.Container) bool {
	for _, resources := range []corev1.ResourceList{container.Resources.Limits, container.Resources.Requests} {
		for name, quantity := range resources {
			if (name == gpuResource || strings.HasPrefix(string(name), migResourcePrefix)) && !quantity.IsZero() {
				return true
			}
		}
	}
	return false
}

func startsWithLauncher(container *corev1.Container) bool {
	prefix := podcontract.CuInterposeLauncherPrefix()
	return len(container.Command) >= len(prefix) && slices.Equal(container.Command[:len(prefix)], prefix)
}
