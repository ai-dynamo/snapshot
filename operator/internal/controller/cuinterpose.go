// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	cuInterposeVolumeName        = "snapshot-cuda"
	cuInterposeInitContainerName = "snapshot-cuda-install"
)

// CuInterposeDelivery describes operator configuration, not workload input.
// It is consulted only when creating an opted-in source Job.
type CuInterposeDelivery struct {
	AgentImage string
	PullPolicy corev1.PullPolicy
}

// ValidatePullPolicy is also called at operator startup so an unsupported flag
// fails immediately, even before any opted-in SnapshotJob exists.
func (d CuInterposeDelivery) ValidatePullPolicy() error {
	switch d.PullPolicy {
	case "", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		return nil
	default:
		return fmt.Errorf("unsupported agent image pull policy %q: use Always, IfNotPresent, Never, or empty", d.PullPolicy)
	}
}

func (d CuInterposeDelivery) validate() error {
	if strings.TrimSpace(d.AgentImage) == "" {
		return fmt.Errorf("cuinterpose requires the Snapshot agent image; configure --agent-image")
	}
	return d.ValidatePullPolicy()
}

// shapeCuInterposeCapture applies delivery once, immediately before Job creation.
// The launcher sees the runtime-resolved environment; Pod environment sources and
// the workload's argument boundaries therefore need no interpretation here.
func shapeCuInterposeCapture(template *corev1.PodTemplateSpec, targetName string, delivery CuInterposeDelivery) error {
	enabled, err := podcontract.ParseCuInterposeAnnotation(template.Annotations)
	if err != nil || !enabled {
		return err
	}
	for _, volume := range template.Spec.Volumes {
		if volume.Name == cuInterposeVolumeName {
			return fmt.Errorf("volume name %q is reserved for cuinterpose", volume.Name)
		}
	}
	for _, containers := range [][]corev1.Container{template.Spec.Containers, template.Spec.InitContainers} {
		for _, container := range containers {
			if container.Name == cuInterposeInitContainerName {
				return fmt.Errorf("container name %q is reserved for cuinterpose", container.Name)
			}
		}
	}
	targetIndex := -1
	for i := range template.Spec.Containers {
		if template.Spec.Containers[i].Name == targetName {
			targetIndex = i
			break
		}
	}
	if targetIndex == -1 {
		return fmt.Errorf("cuinterpose target container %q not found", targetName)
	}
	target := &template.Spec.Containers[targetIndex]
	if len(target.Command) == 0 || target.Command[0] == "" {
		return fmt.Errorf("container %q: cuinterpose requires container.command", targetName)
	}
	for _, mount := range target.VolumeMounts {
		mountPath := path.Clean(mount.MountPath)
		if mount.Name == cuInterposeVolumeName || mountPath == podcontract.CuInterposeMountPath ||
			strings.HasPrefix(mountPath, podcontract.CuInterposeMountPath+"/") {
			return fmt.Errorf("container %q: mount %q at %q conflicts with cuinterpose delivery", targetName, mount.Name, mount.MountPath)
		}
	}
	if err := delivery.validate(); err != nil {
		return err
	}

	shaped := template.DeepCopy()
	target = &shaped.Spec.Containers[targetIndex]
	target.Command = append([]string{podcontract.CuInterposeLauncherPath}, target.Command...)
	target.VolumeMounts = append(target.VolumeMounts, corev1.VolumeMount{
		Name: cuInterposeVolumeName, MountPath: podcontract.CuInterposeMountPath, ReadOnly: true,
	})
	shaped.Spec.Volumes = append(shaped.Spec.Volumes, corev1.Volume{
		Name:         cuInterposeVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	// The agent image defaults to root, but this copy only needs read access to
	// its artifacts and write access to the new emptyDir. Use a numeric nonroot
	// identity so Pods with runAsNonRoot can start the installer too.
	installerUID := int64(65532)
	if security := shaped.Spec.SecurityContext; security != nil &&
		security.RunAsUser != nil && *security.RunAsUser > 0 {
		installerUID = *security.RunAsUser
	}
	shaped.Spec.InitContainers = append(shaped.Spec.InitContainers, corev1.Container{
		Name:            cuInterposeInitContainerName,
		Image:           delivery.AgentImage,
		ImagePullPolicy: delivery.PullPolicy,
		Command:         []string{"/bin/cp"},
		Args: []string{
			"--preserve=mode", "--",
			"/usr/local/lib/snapshot/libcuinterpose.so",
			"/usr/local/lib/snapshot/libcuinterpose_core.so",
			"/usr/local/bin/cuinterpose-launch",
			podcontract.CuInterposeMountPath + "/",
		},
		VolumeMounts: []corev1.VolumeMount{{Name: cuInterposeVolumeName, MountPath: podcontract.CuInterposeMountPath}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To(installerUID),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	})
	*template = *shaped
	return nil
}
