// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"fmt"
	"reflect"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	corev1 "k8s.io/api/core/v1"
)

// EnsureControlVolume adds the snapshot-control emptyDir to the pod spec,
// mounts it on the given container at SnapshotControlMountPath (using
// subPath=<containerName> so concurrent target containers in a failover pod
// each see an isolated view), and sets both SnapshotControlDirEnv (canonical)
// and LegacySnapshotControlDirEnv (deprecated) on the container's env, so
// workload images can migrate off the legacy name independently of the
// operator release. Existing reserved volumes, mounts, and variables must
// match this exact contract; conflicting workload entries are rejected rather
// than silently weakening the control directory's isolation. Idempotent —
// safe to call repeatedly.
//
// Callers must pass the container's own name; the subPath makes the mount
// container-scoped on disk even though the in-container path is the same.
func EnsureControlVolume(podSpec *corev1.PodSpec, container *corev1.Container) error {
	if podSpec == nil || container == nil {
		return fmt.Errorf("snapshot control volume requires a pod spec and target container")
	}

	shapedSpec := podSpec.DeepCopy()
	shapedContainer := container.DeepCopy()

	foundVolume := false
	for i := range shapedSpec.Volumes {
		volume := &shapedSpec.Volumes[i]
		if volume.Name != podcontract.SnapshotControlVolumeName {
			continue
		}
		if foundVolume {
			return fmt.Errorf("duplicate %s volume", podcontract.SnapshotControlVolumeName)
		}
		foundVolume = true
		if volume.EmptyDir == nil ||
			!reflect.DeepEqual(volume.VolumeSource, corev1.VolumeSource{EmptyDir: volume.EmptyDir}) {
			return fmt.Errorf("volume %q must be an emptyDir", podcontract.SnapshotControlVolumeName)
		}
	}
	if !foundVolume {
		shapedSpec.Volumes = append(shapedSpec.Volumes, corev1.Volume{
			Name:         podcontract.SnapshotControlVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}

	// Per-container subPath so each target container has its own sentinel
	// directory on the emptyDir's backing disk. An empty container name
	// degrades to the volume root, which is the correct (and only safe)
	// behavior for single-container pods.
	subPath := container.Name

	foundMount := false
	for i := range shapedContainer.VolumeMounts {
		mount := &shapedContainer.VolumeMounts[i]
		if mount.Name != podcontract.SnapshotControlVolumeName &&
			mount.MountPath != podcontract.SnapshotControlMountPath {
			continue
		}
		if foundMount {
			return fmt.Errorf("container %q has duplicate snapshot control mounts", container.Name)
		}
		foundMount = true
		if mount.Name != podcontract.SnapshotControlVolumeName ||
			mount.MountPath != podcontract.SnapshotControlMountPath ||
			mount.SubPath != subPath ||
			mount.ReadOnly ||
			mount.RecursiveReadOnly != nil ||
			mount.SubPathExpr != "" ||
			(mount.MountPropagation != nil && *mount.MountPropagation != corev1.MountPropagationNone) {
			return fmt.Errorf(
				"container %q requires writable volume %q mounted at %s with subPath %q",
				container.Name,
				podcontract.SnapshotControlVolumeName,
				podcontract.SnapshotControlMountPath,
				subPath,
			)
		}
	}
	if !foundMount {
		shapedContainer.VolumeMounts = append(shapedContainer.VolumeMounts, corev1.VolumeMount{
			Name:      podcontract.SnapshotControlVolumeName,
			MountPath: podcontract.SnapshotControlMountPath,
			SubPath:   subPath,
		})
	}

	for _, name := range []string{
		podcontract.SnapshotControlDirEnv,
		podcontract.LegacySnapshotControlDirEnv,
	} {
		found := false
		for i := range shapedContainer.Env {
			env := &shapedContainer.Env[i]
			if env.Name != name {
				continue
			}
			if found {
				return fmt.Errorf("container %q has duplicate %s environment variables", container.Name, name)
			}
			found = true
			if env.Value != podcontract.SnapshotControlMountPath || env.ValueFrom != nil {
				return fmt.Errorf("container %q has conflicting %s environment variable", container.Name, name)
			}
		}
		if !found {
			shapedContainer.Env = append(
				shapedContainer.Env,
				corev1.EnvVar{Name: name, Value: podcontract.SnapshotControlMountPath},
			)
		}
	}

	podSpec.Volumes = shapedSpec.Volumes
	*container = *shapedContainer
	return nil
}
