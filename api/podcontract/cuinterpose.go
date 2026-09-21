// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

const (
	CuinterposeMountPath         = "/tmp/snapshot-cuda"
	CuinterposeLibraryPath       = CuinterposeMountPath + "/libcuinterpose.so"
	cuinterposeVolumeName        = "snapshot-cuda"
	cuinterposeInitContainerName = "snapshot-cuda-install"
	ldPreloadEnv                 = "LD_PRELOAD"
)

type CuinterposeDelivery struct {
	AgentImage string
	PullPolicy corev1.PullPolicy
}

func CuinterposeEnabled(annotations map[string]string) bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(annotations[CuinterposeAnnotation]))
	return err == nil && enabled
}

// Apply once, before Job creation.
func ShapeCuinterposeCapture(
	podTemplate *corev1.PodTemplateSpec,
	targetContainers []string,
	delivery CuinterposeDelivery,
) error {
	if !CuinterposeEnabled(podTemplate.Annotations) {
		return nil
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
			return fmt.Errorf("container %q not found", name)
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

// The shim must precede other interposers in the loader search order.
func setCuinterposePreload(container *corev1.Container) error {
	index := -1
	for i := range container.Env {
		if container.Env[i].Name != ldPreloadEnv {
			continue
		}
		if index != -1 {
			return fmt.Errorf("container %q has duplicate %s", container.Name, ldPreloadEnv)
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
		return fmt.Errorf("container %q: cannot prepend to %s supplied by valueFrom", container.Name, ldPreloadEnv)
	}
	fields := strings.FieldsFunc(env.Value, func(r rune) bool {
		return r == ':' || unicode.IsSpace(r)
	})
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
