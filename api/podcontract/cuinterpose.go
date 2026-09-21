// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// CuinterposeMountPath must also match the ns-bind-mount helper's destination.
const (
	CuinterposeMountPath         = "/tmp/snapshot-cuda"
	CuinterposeLibraryPath       = CuinterposeMountPath + "/libcuinterpose.so"
	cuinterposeVolumeName        = "snapshot-cuda"
	cuinterposeInitContainerName = "snapshot-cuda-install"
	ldPreloadEnv                 = "LD_PRELOAD"
)

// CuinterposeDelivery selects the agent image that supplies both shim libraries.
type CuinterposeDelivery struct {
	AgentImage string
	PullPolicy corev1.PullPolicy
}

// CuinterposeEnabled reports whether a workload opted into the CUDA interposer
// through CuinterposeAnnotation. An absent annotation disables it; any value
// other than "enabled" is an error rather than a silent no.
func CuinterposeEnabled(annotations map[string]string) (bool, error) {
	raw, found := annotations[CuinterposeAnnotation]
	if !found {
		return false, nil
	}
	if raw != CuinterposeAnnotationEnabled {
		return false, fmt.Errorf("%s must be %q", CuinterposeAnnotation, CuinterposeAnnotationEnabled)
	}
	return true, nil
}

// ShapeCuinterposeCapture installs both shim libraries and preloads the frontend
// for an opted-in source template. Apply once, before creating the Job. Commands and
// existing preload entries are preserved; unannotated templates are untouched.
func ShapeCuinterposeCapture(
	podTemplate *corev1.PodTemplateSpec,
	targetContainers []string,
	delivery CuinterposeDelivery,
) error {
	enabled, err := CuinterposeEnabled(podTemplate.Annotations)
	if err != nil || !enabled {
		return err
	}
	if delivery.AgentImage == "" {
		return fmt.Errorf("cuinterpose requires the Snapshot agent image")
	}
	if len(targetContainers) == 0 {
		return fmt.Errorf("cuinterpose requires at least one target container")
	}
	shaped := podTemplate.DeepCopy()
	for i, name := range targetContainers {
		if slices.Contains(targetContainers[:i], name) {
			continue
		}
		container := findContainer(&shaped.Spec, name)
		if container == nil {
			return fmt.Errorf("cuinterpose target container %q does not exist", name)
		}
		if err := setCuinterposePreload(container); err != nil {
			return err
		}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name: cuinterposeVolumeName, MountPath: CuinterposeMountPath, ReadOnly: true,
		})
	}
	shaped.Spec.Volumes = append(shaped.Spec.Volumes, corev1.Volume{
		Name:         cuinterposeVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	shaped.Spec.InitContainers = append(shaped.Spec.InitContainers, corev1.Container{
		Name:            cuinterposeInitContainerName,
		Image:           delivery.AgentImage,
		ImagePullPolicy: delivery.PullPolicy,
		Command:         []string{"/bin/cp"},
		Args: []string{
			"--",
			"/usr/local/lib/snapshot/libcuinterpose.so",
			"/usr/local/lib/snapshot/libcuinterpose_core.so",
			CuinterposeMountPath + "/",
		},
		VolumeMounts: []corev1.VolumeMount{{Name: cuinterposeVolumeName, MountPath: CuinterposeMountPath}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	})
	*podTemplate = *shaped
	return nil
}

// VerifyCuinterposeCapture prevents adoption of a Job whose target containers
// would run without the requested shim.
func VerifyCuinterposeCapture(spec *corev1.PodSpec, targetContainers []string) error {
	for _, name := range targetContainers {
		container := findContainer(spec, name)
		if container == nil {
			return fmt.Errorf("cuinterpose target container %q does not exist", name)
		}
		if !slices.Contains(preloadFields(envValue(container.Env, ldPreloadEnv)), CuinterposeLibraryPath) {
			return fmt.Errorf("container %q does not preload %s", name, CuinterposeLibraryPath)
		}
		if !slices.ContainsFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.Name == cuinterposeVolumeName && m.MountPath == CuinterposeMountPath
		}) {
			return fmt.Errorf("container %q does not mount %s", name, CuinterposeMountPath)
		}
	}
	return nil
}

// setCuinterposePreload puts the shim first in LD_PRELOAD, keeping any entries
// the workload already had. First position matters: the dynamic loader
// resolves symbols in LD_PRELOAD order, and the shim must see CUDA calls
// before any other interposer.
func setCuinterposePreload(container *corev1.Container) error {
	index := -1
	for i := range container.Env {
		if container.Env[i].Name != ldPreloadEnv {
			continue
		}
		if index != -1 {
			return fmt.Errorf("container %q has duplicate %s environment variables", container.Name, ldPreloadEnv)
		}
		index = i
	}
	if index == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  ldPreloadEnv,
			Value: CuinterposeLibraryPath,
		})
		return nil
	}
	env := &container.Env[index]
	if env.ValueFrom != nil {
		return fmt.Errorf("container %q uses valueFrom for %s", container.Name, ldPreloadEnv)
	}
	fields := preloadFields(env.Value)
	filtered := make([]string, 0, len(fields)+1)
	filtered = append(filtered, CuinterposeLibraryPath)
	for _, field := range fields {
		if field != CuinterposeLibraryPath {
			filtered = append(filtered, field)
		}
	}
	env.Value = strings.Join(filtered, " ")
	return nil
}

func preloadFields(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ':' || unicode.IsSpace(r)
	})
}

func envValue(env []corev1.EnvVar, name string) string {
	for i := range env {
		if env[i].Name == name {
			return env[i].Value
		}
	}
	return ""
}
