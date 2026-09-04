// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
)

// cuinterpose is the CUDA interposer: an LD_PRELOAD library that lets Snapshot
// checkpoint and restore CUDA memory shared between processes on one node. A
// Pod opts in with CuinterposeAnnotation. The shim travels with the CUDA tools
// delivery (cudatools.go); shaping puts it first in LD_PRELOAD and launches
// every target under cuda-checkpoint --launch-job whatever its GPU count,
// because the shim's checkpoint path needs the CUDA job file.
const ldPreloadEnv = "LD_PRELOAD"

// CuinterposeEnabled reports whether a workload opted into the CUDA interposer
// through CuinterposeAnnotation. An absent annotation disables it; any value
// other than "enabled" is an error rather than a silent no.
func CuinterposeEnabled(annotations map[string]string) (bool, error) {
	raw, found := annotations[CuinterposeAnnotation]
	if !found {
		return false, nil
	}
	if raw != strings.TrimSpace(raw) {
		return false, fmt.Errorf("%s must not contain surrounding whitespace", CuinterposeAnnotation)
	}
	if raw != CuinterposeAnnotationEnabled {
		return false, fmt.Errorf("%s must be %q", CuinterposeAnnotation, CuinterposeAnnotationEnabled)
	}
	return true, nil
}

// ShapeCuinterposeCapture installs the capture-side contract on every target
// container when the template opts in: the CUDA tools delivery and launch-job
// wrapper (ShapeCUDALaunchJob's contract, applied regardless of GPU count) plus
// LD_PRELOAD pointing at the shim. Reapplying to an already shaped template is
// a no-op. On error the template is left unchanged.
func ShapeCuinterposeCapture(
	podTemplate *corev1.PodTemplateSpec,
	targetContainers []string,
	delivery CUDAToolsDelivery,
) error {
	if podTemplate == nil {
		return fmt.Errorf("cuinterpose requires a pod template")
	}
	enabled, err := CuinterposeEnabled(podTemplate.Annotations)
	if err != nil || !enabled {
		return err
	}
	if len(targetContainers) == 0 {
		return fmt.Errorf("cuinterpose requires at least one target container")
	}
	shaped := podTemplate.DeepCopy()
	targets := uniqueNames(targetContainers)
	if err := shapeCUDALaunchJob(shaped, targets, delivery); err != nil {
		return fmt.Errorf("cuinterpose: %w", err)
	}
	for _, name := range targets {
		if err := setCuinterposePreload(findContainer(&shaped.Spec, name)); err != nil {
			return err
		}
	}
	*podTemplate = *shaped
	return nil
}

// VerifyCuinterposeCapture reports whether spec already carries the complete
// capture contract for every target container: the launch-job contract and
// the LD_PRELOAD entry. The operator uses it to refuse adopting a Job that was
// created without the shim.
func VerifyCuinterposeCapture(spec *corev1.PodSpec, targetContainers []string) error {
	if err := VerifyCUDALaunchJob(spec, targetContainers); err != nil {
		return err
	}
	for _, name := range targetContainers {
		container := findContainer(spec, name)
		if !slices.Contains(preloadFields(envValue(container.Env, ldPreloadEnv)), CuinterposeLibraryPath) {
			return fmt.Errorf("container %q does not preload %s", name, CuinterposeLibraryPath)
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
