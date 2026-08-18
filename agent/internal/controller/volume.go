// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func declaredVolumesForContainer(pod *corev1.Pod, containerName string) []snapshotv1alpha1.CheckpointSourceDeclaredVolume {
	volumes := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, volume := range pod.Spec.Volumes {
		volumes[volume.Name] = volume
	}

	for _, container := range pod.Spec.Containers {
		if container.Name != containerName {
			continue
		}

		declaredVolumes := make([]snapshotv1alpha1.CheckpointSourceDeclaredVolume, 0, len(container.VolumeMounts))
		for _, volumeMount := range container.VolumeMounts {
			volume, found := volumes[volumeMount.Name]
			volumeSource := "Volume/" + volumeMount.Name
			if found {
				volumeSource = describeVolumeSource(volume)
			}
			declaredVolumes = append(declaredVolumes, snapshotv1alpha1.CheckpointSourceDeclaredVolume{
				Path:         volumeMount.MountPath,
				Volume:       volumeMount.Name,
				VolumeSource: volumeSource,
			})
		}
		return declaredVolumes
	}

	return nil
}

func describeVolumeSource(volume corev1.Volume) string {
	switch {
	case volume.PersistentVolumeClaim != nil:
		return namedVolumeSource("PersistentVolumeClaim", volume.PersistentVolumeClaim.ClaimName)
	case volume.ConfigMap != nil:
		return namedVolumeSource("ConfigMap", volume.ConfigMap.Name)
	case volume.Secret != nil:
		return namedVolumeSource("Secret", volume.Secret.SecretName)
	case volume.HostPath != nil:
		return namedVolumeSource("HostPath", volume.HostPath.Path)
	case volume.CSI != nil:
		return namedVolumeSource("CSI", volume.CSI.Driver)
	case volume.NFS != nil:
		return namedVolumeSource("NFS", volume.NFS.Server)
	case volume.EmptyDir != nil:
		return "EmptyDir"
	case volume.Projected != nil:
		return "Projected"
	case volume.DownwardAPI != nil:
		return "DownwardAPI"
	case volume.Ephemeral != nil:
		return "Ephemeral"
	default:
		return "Volume/" + volume.Name
	}
}

func namedVolumeSource(kind, name string) string {
	if name == "" {
		return kind
	}
	return kind + "/" + name
}
