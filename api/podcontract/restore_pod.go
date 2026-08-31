// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const restoreStartupFailureThreshold int32 = 1800 // 30 minutes at 1s cadence.

// Request identifies the PodSnapshot source and its restore destinations.
// Empty Mappings default to restoring SourceContainer into a same-named
// destination container.
type Request struct {
	SnapshotName    string
	SourceContainer string
	Mappings        []ContainerMapping
}

// Options controls optional restore Pod shaping. Its zero value applies only
// the mechanics required by Snapshot's minimum restore protocol.
type Options struct {
	// SeccompProfile is the kubelet-local profile applied to each restore
	// destination container. Empty leaves seccomp configuration entirely
	// caller-owned.
	SeccompProfile string

	// EnableStartupGate replaces each destination container's startup probe
	// with a restore-completion gate. Workload owners opt in when Kubernetes
	// must withhold readiness and liveness until restore completes.
	EnableStartupGate bool
}

// Build returns a restore-shaped deep copy of pod. The caller's Pod
// is never mutated, including when validation fails. Workload-specific standby
// behavior and container commands remain the caller's responsibility. Mapping
// sources must come from the referenced PodSnapshot; this pure builder performs
// no API read to derive them.
func Build(pod *corev1.Pod, request Request, options Options) (*corev1.Pod, error) {
	if pod == nil {
		return nil, fmt.Errorf("restore pod is nil")
	}
	request, err := normalizeRequest(request)
	if err != nil {
		return nil, err
	}
	result := pod.DeepCopy()
	if err := ensureRestoreAnnotations(result, request); err != nil {
		return nil, err
	}
	if err := ensureRestorePodSpec(&result.Spec, request.Mappings, options); err != nil {
		return nil, err
	}
	if err := validateCanonicalRestorePod(result, request, options); err != nil {
		return nil, fmt.Errorf("validate shaped restore pod: %w", err)
	}
	return result, nil
}

// Validate verifies that pod implements Snapshot's minimum restore protocol.
// Optional workload policy such as startup gating and seccomp selection is not
// part of this agent-facing validation. Validate performs no API reads and
// never mutates the Pod.
func Validate(pod *corev1.Pod, request Request) error {
	request, err := normalizeRequest(request)
	if err != nil {
		return err
	}
	return validateMinimumRestorePod(pod, request)
}

func validateMinimumRestorePod(pod *corev1.Pod, request Request) error {
	if pod == nil {
		return fmt.Errorf("restore pod is nil")
	}
	if err := validateRestoreAnnotations(pod.Annotations, request); err != nil {
		return err
	}
	if err := validateControlVolume(&pod.Spec); err != nil {
		return err
	}
	for _, mapping := range request.Mappings {
		container := findContainer(&pod.Spec, mapping.Destination)
		if container == nil {
			return fmt.Errorf("restore pod has no destination container named %q", mapping.Destination)
		}
		if err := validateControlMount(container); err != nil {
			return err
		}
		if err := validateControlEnvironment(container); err != nil {
			return err
		}
	}
	return nil
}

func validateCanonicalRestorePod(pod *corev1.Pod, request Request, options Options) error {
	if err := validateMinimumRestorePod(pod, request); err != nil {
		return err
	}
	for _, mapping := range request.Mappings {
		container := findContainer(&pod.Spec, mapping.Destination)
		if options.EnableStartupGate {
			if err := validateCanonicalRestoreStartupProbe(container); err != nil {
				return err
			}
		}
		if err := validateContainerSeccompProfile(container, options.SeccompProfile); err != nil {
			return err
		}
	}
	return nil
}

func normalizeRequest(request Request) (Request, error) {
	snapshotName, err := validateRestoreFromSnapshotName(request.SnapshotName)
	if err != nil {
		return Request{}, err
	}
	source := strings.TrimSpace(request.SourceContainer)
	mappings := request.Mappings
	if len(mappings) == 0 {
		mappings = []ContainerMapping{{Source: source, Destination: source}}
	}
	if err := ValidateContainerMappings(mappings, source); err != nil {
		return Request{}, err
	}
	request.SnapshotName = snapshotName
	request.SourceContainer = source
	request.Mappings = append([]ContainerMapping(nil), mappings...)
	return request, nil
}

func ensureRestoreAnnotations(pod *corev1.Pod, request Request) error {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string, 2)
	}
	if existing, found := pod.Annotations[RestoreFromAnnotation]; found {
		resolved, err := validateRestoreFromSnapshotName(existing)
		if err != nil {
			return err
		}
		if resolved != request.SnapshotName {
			return fmt.Errorf(
				"%s names %q, conflicting with requested PodSnapshot %q",
				RestoreFromAnnotation,
				resolved,
				request.SnapshotName,
			)
		}
	}
	pod.Annotations[RestoreFromAnnotation] = request.SnapshotName

	formatted, needsMapping := formatContainerMappings(request.Mappings)
	if _, found := pod.Annotations[RestoreContainerMapAnnotation]; found {
		existingMappings, err := ContainerMappingsFromAnnotations(pod.Annotations, request.SourceContainer)
		if err != nil {
			return err
		}
		if err := ValidateContainerMappings(existingMappings, request.SourceContainer); err != nil {
			return err
		}
		if !sameContainerMappings(existingMappings, request.Mappings) {
			return fmt.Errorf("%s conflicts with requested restore mappings", RestoreContainerMapAnnotation)
		}
		if needsMapping {
			pod.Annotations[RestoreContainerMapAnnotation] = formatted
		} else {
			delete(pod.Annotations, RestoreContainerMapAnnotation)
		}
		return nil
	}
	if needsMapping {
		pod.Annotations[RestoreContainerMapAnnotation] = formatted
	}
	return nil
}

func validateRestoreAnnotations(annotations map[string]string, request Request) error {
	resolved, err := GetRestoreFromSnapshotName(annotations)
	if err != nil {
		return err
	}
	if resolved != request.SnapshotName {
		return fmt.Errorf("%s names %q, expected %q", RestoreFromAnnotation, resolved, request.SnapshotName)
	}
	annotatedMappings, err := ContainerMappingsFromAnnotations(annotations, request.SourceContainer)
	if err != nil {
		return err
	}
	if err := ValidateContainerMappings(annotatedMappings, request.SourceContainer); err != nil {
		return err
	}
	if !sameContainerMappings(annotatedMappings, request.Mappings) {
		return fmt.Errorf("%s does not match the requested restore mappings", RestoreContainerMapAnnotation)
	}
	return nil
}

func formatContainerMappings(mappings []ContainerMapping) (string, bool) {
	if len(mappings) == 1 && mappings[0].Source == mappings[0].Destination {
		return "", false
	}
	formatted := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		formatted = append(formatted, mapping.Source+"="+mapping.Destination)
	}
	sort.Strings(formatted)
	return strings.Join(formatted, ","), true
}

func sameContainerMappings(left, right []ContainerMapping) bool {
	if len(left) != len(right) {
		return false
	}
	want := make(map[ContainerMapping]struct{}, len(right))
	for _, mapping := range right {
		want[mapping] = struct{}{}
	}
	for _, mapping := range left {
		if _, found := want[mapping]; !found {
			return false
		}
	}
	return true
}

func ensureRestorePodSpec(spec *corev1.PodSpec, mappings []ContainerMapping, options Options) error {
	if err := ensureControlVolume(spec); err != nil {
		return err
	}
	for _, mapping := range mappings {
		container := findContainer(spec, mapping.Destination)
		if container == nil {
			return fmt.Errorf("restore pod has no destination container named %q", mapping.Destination)
		}
		if err := ensureContainerSeccompProfile(container, options.SeccompProfile); err != nil {
			return err
		}
		if err := ensureControlMount(container); err != nil {
			return err
		}
		if err := ensureControlEnvironment(container); err != nil {
			return err
		}
		if options.EnableStartupGate {
			ensureRestoreStartupProbe(container)
		}
	}
	return nil
}

func findContainer(spec *corev1.PodSpec, name string) *corev1.Container {
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			return &spec.Containers[i]
		}
	}
	return nil
}

func ensureControlVolume(spec *corev1.PodSpec) error {
	found, err := hasValidControlVolume(spec)
	if err != nil {
		return err
	}
	if !found {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         SnapshotControlVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	return nil
}

func validateControlVolume(spec *corev1.PodSpec) error {
	found, err := hasValidControlVolume(spec)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("missing %s emptyDir volume", SnapshotControlVolumeName)
	}
	return nil
}

func hasValidControlVolume(spec *corev1.PodSpec) (bool, error) {
	found := false
	for i := range spec.Volumes {
		volume := &spec.Volumes[i]
		if volume.Name != SnapshotControlVolumeName {
			continue
		}
		if found {
			return false, fmt.Errorf("duplicate %s volume", SnapshotControlVolumeName)
		}
		found = true
		if !isEmptyDirVolumeSource(volume.VolumeSource) {
			return false, fmt.Errorf("volume %q must be an emptyDir", SnapshotControlVolumeName)
		}
	}
	return found, nil
}

func isEmptyDirVolumeSource(source corev1.VolumeSource) bool {
	return source.EmptyDir != nil && reflect.DeepEqual(source, corev1.VolumeSource{EmptyDir: source.EmptyDir})
}

func ensureControlMount(container *corev1.Container) error {
	found, err := hasValidControlMount(container)
	if err != nil {
		return err
	}
	if !found {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      SnapshotControlVolumeName,
			MountPath: SnapshotControlMountPath,
			SubPath:   container.Name,
		})
	}
	return nil
}

func validateControlMount(container *corev1.Container) error {
	found, err := hasValidControlMount(container)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf(
			"container %q is missing %s mounted at %s",
			container.Name,
			SnapshotControlVolumeName,
			SnapshotControlMountPath,
		)
	}
	return nil
}

func hasValidControlMount(container *corev1.Container) (bool, error) {
	found := false
	for i := range container.VolumeMounts {
		mount := &container.VolumeMounts[i]
		if mount.Name != SnapshotControlVolumeName && mount.MountPath != SnapshotControlMountPath {
			continue
		}
		if found {
			return false, fmt.Errorf("container %q has duplicate snapshot control mounts", container.Name)
		}
		found = true
		if err := validateControlMountValue(container.Name, mount); err != nil {
			return false, err
		}
	}
	return found, nil
}

func validateControlMountValue(containerName string, mount *corev1.VolumeMount) error {
	if mount.Name != SnapshotControlVolumeName ||
		mount.MountPath != SnapshotControlMountPath ||
		mount.SubPath != containerName {
		return fmt.Errorf(
			"container %q requires volume %q mounted at %s with subPath %q",
			containerName,
			SnapshotControlVolumeName,
			SnapshotControlMountPath,
			containerName,
		)
	}
	if mount.ReadOnly || mount.RecursiveReadOnly != nil || mount.SubPathExpr != "" ||
		(mount.MountPropagation != nil && *mount.MountPropagation != corev1.MountPropagationNone) {
		return fmt.Errorf("container %q has conflicting options on the snapshot control mount", containerName)
	}
	return nil
}

func ensureControlEnvironment(container *corev1.Container) error {
	for _, name := range []string{SnapshotControlDirEnv, LegacySnapshotControlDirEnv} {
		if err := ensureControlEnv(container, name); err != nil {
			return err
		}
	}
	return nil
}

func ensureControlEnv(container *corev1.Container, name string) error {
	found, err := hasValidControlEnv(container, name)
	if err != nil {
		return err
	}
	if !found {
		container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: SnapshotControlMountPath})
	}
	return nil
}

func validateControlEnvironment(container *corev1.Container) error {
	found, err := hasValidControlEnv(container, SnapshotControlDirEnv)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("container %q is missing %s environment variable", container.Name, SnapshotControlDirEnv)
	}
	if _, err := hasValidControlEnv(container, LegacySnapshotControlDirEnv); err != nil {
		return err
	}
	return nil
}

func hasValidControlEnv(container *corev1.Container, name string) (bool, error) {
	found := false
	for i := range container.Env {
		env := &container.Env[i]
		if env.Name != name {
			continue
		}
		if found {
			return false, fmt.Errorf("container %q has duplicate %s environment variables", container.Name, name)
		}
		found = true
		if env.Value != SnapshotControlMountPath || env.ValueFrom != nil {
			return false, fmt.Errorf("container %q has conflicting %s environment variable", container.Name, name)
		}
	}
	return found, nil
}

func ensureRestoreStartupProbe(container *corev1.Container) {
	container.StartupProbe = canonicalRestoreStartupProbe()
}

func canonicalRestoreStartupProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"cat", path.Join(SnapshotControlMountPath, RestoreCompleteFile)},
		}},
		TimeoutSeconds:   1,
		PeriodSeconds:    1,
		FailureThreshold: restoreStartupFailureThreshold,
		SuccessThreshold: 1,
	}
}

func validateCanonicalRestoreStartupProbe(container *corev1.Container) error {
	if !reflect.DeepEqual(container.StartupProbe, canonicalRestoreStartupProbe()) {
		return fmt.Errorf("container %q has a conflicting restore startup gate", container.Name)
	}
	return nil
}

func ensureContainerSeccompProfile(container *corev1.Container, expected string) error {
	if expected == "" {
		return nil
	}
	if container.SecurityContext == nil {
		container.SecurityContext = &corev1.SecurityContext{}
	}
	if container.SecurityContext.SeccompProfile == nil {
		container.SecurityContext.SeccompProfile = localhostSeccompProfile(expected)
		return nil
	}
	return validateContainerSeccompProfile(container, expected)
}

func validateContainerSeccompProfile(container *corev1.Container, expected string) error {
	if expected == "" {
		return nil
	}
	if container.SecurityContext == nil ||
		!matchesLocalhostSeccompProfile(container.SecurityContext.SeccompProfile, expected) {
		return fmt.Errorf("container %q must use localhost seccomp profile %q", container.Name, expected)
	}
	return nil
}

func localhostSeccompProfile(profile string) *corev1.SeccompProfile {
	return &corev1.SeccompProfile{
		Type:             corev1.SeccompProfileTypeLocalhost,
		LocalhostProfile: &profile,
	}
}

func matchesLocalhostSeccompProfile(profile *corev1.SeccompProfile, expected string) bool {
	return profile != nil && profile.Type == corev1.SeccompProfileTypeLocalhost &&
		profile.LocalhostProfile != nil && *profile.LocalhostProfile == expected
}
