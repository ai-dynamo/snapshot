// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"reflect"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

const (
	CUDAInterposerVolumeName        = "cuda-interposer"
	CUDAInterposerInitContainerName = "cuda-interposer-install"
	CUDAInterposerMountPath         = "/tmp/snapshot-interposer"
	CUDAInterposerLibraryPath       = CUDAInterposerMountPath + "/libcuinterposer.so"

	cudaInterposerImageLibraryPath = "/usr/local/lib/snapshot/libcuinterposer.so"
	cudaInterposerInstallMountPath = "/cuda-interposer"
	cudaInterposerInstallPath      = cudaInterposerInstallMountPath + "/libcuinterposer.so"
	ldPreloadEnv                   = "LD_PRELOAD"
)

// CUDAInterposerEnabled reports whether a workload opted into CUDA
// interposition. An absent annotation disables it.
func CUDAInterposerEnabled(annotations map[string]string) (bool, error) {
	raw, found := annotations[CUDAInterposerAnnotation]
	if !found {
		return false, nil
	}
	if raw != strings.TrimSpace(raw) {
		return false, fmt.Errorf("%s must not contain surrounding whitespace", CUDAInterposerAnnotation)
	}
	if raw != "enabled" {
		return false, fmt.Errorf("%s must be %q", CUDAInterposerAnnotation, "enabled")
	}
	return true, nil
}

// ShapeCUDAInterposerCapture installs the capture-only interposer contract on
// every target container in podTemplate. The caller supplies the Snapshot
// installation's configured agent image; workload users only opt in through
// CUDAInterposerAnnotation.
func ShapeCUDAInterposerCapture(
	podTemplate *corev1.PodTemplateSpec,
	targetContainers []string,
	agentImage string,
) error {
	if podTemplate == nil {
		return fmt.Errorf("CUDA interposer requires a pod template")
	}
	shaped := podTemplate.DeepCopy()
	if err := shapeCUDAInterposerCapture(shaped, targetContainers, agentImage); err != nil {
		return err
	}
	*podTemplate = *shaped
	return nil
}

func shapeCUDAInterposerCapture(
	podTemplate *corev1.PodTemplateSpec,
	targetContainers []string,
	agentImage string,
) error {
	enabled, err := CUDAInterposerEnabled(podTemplate.Annotations)
	if err != nil || !enabled {
		return err
	}
	agentImage = strings.TrimSpace(agentImage)
	if agentImage == "" {
		return fmt.Errorf("CUDA interposer requires the configured Snapshot agent image")
	}
	if len(targetContainers) == 0 {
		return fmt.Errorf("CUDA interposer requires at least one target container")
	}
	if err := ensureCUDAInterposerVolume(&podTemplate.Spec); err != nil {
		return err
	}
	if err := ensureCUDAInterposerInitContainer(&podTemplate.Spec, agentImage); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(targetContainers))
	for _, name := range targetContainers {
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate CUDA interposer target container %q", name)
		}
		seen[name] = struct{}{}
		container := findContainer(&podTemplate.Spec, name)
		if container == nil {
			return fmt.Errorf("CUDA interposer target container %q does not exist", name)
		}
		if err := ensureCUDAInterposerMount(container); err != nil {
			return err
		}
		if err := setCUDAInterposerPreload(container); err != nil {
			return err
		}
	}
	return nil
}

func ensureCUDAInterposerVolume(spec *corev1.PodSpec) error {
	found := false
	for i := range spec.Volumes {
		volume := &spec.Volumes[i]
		if volume.Name != CUDAInterposerVolumeName {
			continue
		}
		if found {
			return fmt.Errorf("duplicate %s volume", CUDAInterposerVolumeName)
		}
		found = true
		if !isEmptyDirVolumeSource(volume.VolumeSource) {
			return fmt.Errorf("volume %q must be an emptyDir", CUDAInterposerVolumeName)
		}
	}
	if !found {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         CUDAInterposerVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	return nil
}

func expectedCUDAInterposerInitContainer(agentImage string) corev1.Container {
	pullPolicy := corev1.PullAlways
	if strings.Contains(agentImage, "@sha256:") {
		pullPolicy = corev1.PullIfNotPresent
	}
	return corev1.Container{
		Name:            CUDAInterposerInitContainerName,
		Image:           agentImage,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/bin/cp"},
		Args:            []string{"--", cudaInterposerImageLibraryPath, cudaInterposerInstallPath},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      CUDAInterposerVolumeName,
			MountPath: cudaInterposerInstallMountPath,
		}},
	}
}

func ensureCUDAInterposerInitContainer(spec *corev1.PodSpec, agentImage string) error {
	expected := expectedCUDAInterposerInitContainer(agentImage)
	found := false
	for i := range spec.InitContainers {
		container := &spec.InitContainers[i]
		if container.Name != CUDAInterposerInitContainerName {
			continue
		}
		if found {
			return fmt.Errorf("duplicate %s init container", CUDAInterposerInitContainerName)
		}
		found = true
		if !reflect.DeepEqual(*container, expected) {
			return fmt.Errorf(
				"init container %q conflicts with the CUDA interposer contract",
				CUDAInterposerInitContainerName,
			)
		}
	}
	if !found {
		spec.InitContainers = append(spec.InitContainers, expected)
	}
	return nil
}

func ensureCUDAInterposerMount(container *corev1.Container) error {
	found := false
	for i := range container.VolumeMounts {
		mount := &container.VolumeMounts[i]
		if mount.Name != CUDAInterposerVolumeName && mount.MountPath != CUDAInterposerMountPath {
			continue
		}
		if found {
			return fmt.Errorf("container %q has duplicate CUDA interposer mounts", container.Name)
		}
		found = true
		if mount.Name != CUDAInterposerVolumeName || mount.MountPath != CUDAInterposerMountPath ||
			!mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" ||
			mount.RecursiveReadOnly != nil ||
			(mount.MountPropagation != nil && *mount.MountPropagation != corev1.MountPropagationNone) {
			return fmt.Errorf("container %q has conflicting options on the CUDA interposer mount", container.Name)
		}
	}
	if !found {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      CUDAInterposerVolumeName,
			MountPath: CUDAInterposerMountPath,
			ReadOnly:  true,
		})
	}
	return nil
}

func setCUDAInterposerPreload(container *corev1.Container) error {
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
			Value: CUDAInterposerLibraryPath,
		})
		return nil
	}
	env := &container.Env[index]
	if env.ValueFrom != nil {
		return fmt.Errorf("container %q uses valueFrom for %s", container.Name, ldPreloadEnv)
	}
	fields := preloadFields(env.Value)
	filtered := make([]string, 0, len(fields)+1)
	filtered = append(filtered, CUDAInterposerLibraryPath)
	for _, field := range fields {
		if field != CUDAInterposerLibraryPath {
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
