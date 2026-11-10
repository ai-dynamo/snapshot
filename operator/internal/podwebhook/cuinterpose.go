// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package podwebhook serves the operator's mutating admission webhook for Pods.
//
// A Pod that opts into multi-GPU support with the nvidia.com/snapshot-multi-gpu
// annotation needs the cuInterpose libraries installed and its checkpoint targets
// started under the launcher before any CUDA call. On nodes with NRI the agent's
// plugin does this when each container starts. Without NRI the operator runs with
// --multi-gpu-preparation=initContainer and shapes SnapshotJob sources itself;
// this webhook shapes every other opted-in Pod the same way, whether created
// directly or through a Deployment.
//
// It picks targets by the rule the agent's plugin uses: a container that mounts
// /snapshot-control, unless it delivers the libraries itself. Each target must
// set an explicit command: admission runs before any runtime resolves the image,
// so the launcher has nothing to wrap otherwise.
package podwebhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"slices"

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
	original := pod.Spec.DeepCopy()
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
	shaped, err := applyShaping(req.Object.Raw, original, &pod.Spec, outcome.targets)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	log.FromContext(ctx).Info("prepared Pod for cuinterpose", "containers", outcome.targets)
	return admission.PatchResponseFromRaw(req.Object.Raw, shaped)
}

// applyShaping carries the difference between original and shaped over to the
// Pod as the API server serialized it, and returns that document. Re-encoding
// the decoded Go struct instead would drop every field the operator's API types
// do not know, such as one a newer API server adds, and the patch would remove it
// from the Pod. So the raw document is edited in place: the new volume, init
// container and volume mounts are appended to the existing arrays, and only the
// targets' command is replaced. Shaping appends and never reorders, so the new
// entries are the suffix of each shaped slice.
func applyShaping(raw []byte, original, shaped *corev1.PodSpec, targets []string) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("decode admitted Pod: %w", err)
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(object["spec"], &spec); err != nil {
		return nil, fmt.Errorf("decode admitted Pod spec: %w", err)
	}
	if err := appendRaw(spec, "volumes", shaped.Volumes[len(original.Volumes):]); err != nil {
		return nil, err
	}
	if err := appendRaw(spec, "initContainers", shaped.InitContainers[len(original.InitContainers):]); err != nil {
		return nil, err
	}
	var containers []json.RawMessage
	if err := json.Unmarshal(spec["containers"], &containers); err != nil {
		return nil, fmt.Errorf("decode admitted containers: %w", err)
	}
	for index, rawContainer := range containers {
		var named struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(rawContainer, &named); err != nil {
			return nil, fmt.Errorf("decode admitted container: %w", err)
		}
		if !slices.Contains(targets, named.Name) {
			continue
		}
		var container map[string]json.RawMessage
		if err := json.Unmarshal(rawContainer, &container); err != nil {
			return nil, fmt.Errorf("decode admitted container %q: %w", named.Name, err)
		}
		before, after := containerByName(original, named.Name), containerByName(shaped, named.Name)
		command, err := json.Marshal(after.Command)
		if err != nil {
			return nil, err
		}
		container["command"] = command
		if err := appendRaw(container, "volumeMounts", after.VolumeMounts[len(before.VolumeMounts):]); err != nil {
			return nil, err
		}
		if containers[index], err = json.Marshal(container); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(containers)
	if err != nil {
		return nil, err
	}
	spec["containers"] = encoded
	if object["spec"], err = json.Marshal(spec); err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

// appendRaw appends items to the JSON array at object[key], creating it when
// absent, without re-encoding the existing elements.
func appendRaw[T any](object map[string]json.RawMessage, key string, items []T) error {
	if len(items) == 0 {
		return nil
	}
	var array []json.RawMessage
	if existing, ok := object[key]; ok {
		if err := json.Unmarshal(existing, &array); err != nil {
			return fmt.Errorf("decode admitted %s: %w", key, err)
		}
	}
	for _, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return err
		}
		array = append(array, encoded)
	}
	encoded, err := json.Marshal(array)
	if err != nil {
		return err
	}
	object[key] = encoded
	return nil
}

func containerByName(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
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
	checkpointTargets, targets := cuInterposeTargets(&pod.Spec)
	switch {
	case len(checkpointTargets) == 0:
		return shapeOutcome{
			reason: "no container mounts " + podcontract.SnapshotControlMountPath,
			warning: fmt.Sprintf("%s is enabled but no container mounts %s, so none was prepared for cuinterpose",
				podcontract.CuInterposeAnnotation, podcontract.SnapshotControlMountPath),
		}, nil
	case len(targets) == 0:
		// The Pod delivers the libraries its own way, as the workload contract allows.
		return shapeOutcome{reason: "every checkpoint target delivers cuinterpose itself"}, nil
	}
	if err := protocol.ShapeCuInterposeCapture(&pod.Spec, targets, i.Installer); err != nil {
		return shapeOutcome{}, err
	}
	log.FromContext(ctx).V(1).Info("shaped Pod for cuinterpose", "containers", targets)
	return shapeOutcome{changed: true, targets: targets}, nil
}

// cuInterposeTargets names the checkpoint targets, the containers that mount
// /snapshot-control, and among them the ones to prepare: those that do not deliver
// cuinterpose themselves. A container delivers it itself when it already starts
// the launcher or mounts something at the delivery path. This is the rule the
// agent's NRI plugin applies, so a container is prepared the same way whichever
// of the two sees it.
func cuInterposeTargets(spec *corev1.PodSpec) (checkpointTargets, targets []string) {
	for i := range spec.Containers {
		container := &spec.Containers[i]
		if !mountsControlDir(container) {
			continue
		}
		checkpointTargets = append(checkpointTargets, container.Name)
		if !deliversCuInterpose(container) {
			targets = append(targets, container.Name)
		}
	}
	return checkpointTargets, targets
}

func mountsControlDir(container *corev1.Container) bool {
	return slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool {
		return path.Clean(mount.MountPath) == podcontract.SnapshotControlMountPath
	})
}

func deliversCuInterpose(container *corev1.Container) bool {
	if podcontract.HasCuInterposeLauncher(append(slices.Clone(container.Command), container.Args...)) {
		return true
	}
	return slices.ContainsFunc(container.VolumeMounts, func(mount corev1.VolumeMount) bool {
		return podcontract.IsCuInterposeDeliveryPath(mount.MountPath)
	})
}
