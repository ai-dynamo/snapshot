// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// EnsureControlVolume adds the snapshot-control emptyDir to the pod spec,
// mounts it on the given container at SnapshotControlMountPath (using
// subPath=<containerName> so concurrent target containers in a failover pod
// each see an isolated view), and sets both SnapshotControlDirEnv (canonical)
// and LegacySnapshotControlDirEnv (deprecated) on the container's env, so
// workload images can migrate off the legacy name independently of the
// operator release. Idempotent — safe to call from multiple code paths
// (operator source job, restore pod shaping, etc.); each env var is
// guarded independently so a pod that already carries one (e.g. a
// hand-crafted template with only the legacy name) still gets the other
// injected without duplicating either.
//
// Callers must pass the container's own name; the subPath makes the mount
// container-scoped on disk even though the in-container path is the same.
func EnsureControlVolume(podSpec *corev1.PodSpec, container *corev1.Container) {
	if podSpec == nil || container == nil {
		return
	}

	hasVolume := false
	for _, v := range podSpec.Volumes {
		if v.Name == snapshotv1alpha1.SnapshotControlVolumeName {
			hasVolume = true
			break
		}
	}
	if !hasVolume {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name:         snapshotv1alpha1.SnapshotControlVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	// Per-container subPath so each target container has its own sentinel
	// directory on the emptyDir's backing disk. An empty container name
	// degrades to the volume root, which is the correct (and only safe)
	// behavior for single-container pods.
	subPath := container.Name

	hasMount := false
	for _, m := range container.VolumeMounts {
		if m.Name == snapshotv1alpha1.SnapshotControlVolumeName {
			hasMount = true
			break
		}
	}
	if !hasMount {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      snapshotv1alpha1.SnapshotControlVolumeName,
			MountPath: snapshotv1alpha1.SnapshotControlMountPath,
			SubPath:   subPath,
		})
	}

	ensureEnv(container, snapshotv1alpha1.SnapshotControlDirEnv, snapshotv1alpha1.SnapshotControlMountPath)
	ensureEnv(container, snapshotv1alpha1.LegacySnapshotControlDirEnv, snapshotv1alpha1.SnapshotControlMountPath)
}

// ensureEnv sets name=value on the container if name is not already present,
// so repeated calls (e.g. dual-injecting the canonical and legacy control-dir
// env var names) never duplicate an entry.
func ensureEnv(container *corev1.Container, name, value string) {
	for _, e := range container.Env {
		if e.Name == name {
			return
		}
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: value})
}
