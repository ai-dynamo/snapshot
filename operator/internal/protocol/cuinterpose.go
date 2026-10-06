// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

const (
	cuInterposeVolumeName        = "snapshot-cuda"
	cuInterposeInitContainerName = "snapshot-cuda-install"

	// The cuinterpose payload paths inside the configured image.
	cuInterposeImageFrontend = "/usr/local/lib/snapshot/libcuinterpose.so"
	cuInterposeImageCore     = "/usr/local/lib/snapshot/libcuinterpose_core.so"
	cuInterposeImageLauncher = "/usr/local/bin/cuinterpose-launch"

	// The image defaults to root, but the copy needs only read access to the payload
	// and write access to the new emptyDir.
	cuInterposeInstallerUID = int64(65532)
)

// ShapeCuInterposeCapture prepares an opted-in source Pod template once, immediately
// before Job creation. It installs the libraries and launcher before startup and
// starts the target under the launcher, which receives the runtime-resolved
// environment, so Pod environment sources and argument boundaries stay unchanged. It
// validates everything before changing the template.
func ShapeCuInterposeCapture(template *corev1.PodTemplateSpec, targetName string, installer operatortypes.CuInterposeContainerConfiguration) error {
	if err := checkCuInterposeReservedNames(&template.Spec); err != nil {
		return err
	}
	target, err := cuInterposeTarget(&template.Spec, targetName)
	if err != nil {
		return err
	}
	target.Command = append(podcontract.CuInterposeLauncherPrefix(), target.Command...)
	target.VolumeMounts = append(target.VolumeMounts, corev1.VolumeMount{
		Name: cuInterposeVolumeName, MountPath: podcontract.CuInterposeMountPath, ReadOnly: true,
	})
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
		Name:         cuInterposeVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	template.Spec.InitContainers = append(template.Spec.InitContainers, cuInterposeInstaller(template.Spec.SecurityContext, installer))
	return nil
}

func checkCuInterposeReservedNames(spec *corev1.PodSpec) error {
	for _, volume := range spec.Volumes {
		if volume.Name == cuInterposeVolumeName {
			return fmt.Errorf("volume name %q is reserved for cuinterpose", volume.Name)
		}
	}
	for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for _, container := range containers {
			if container.Name == cuInterposeInitContainerName {
				return fmt.Errorf("container name %q is reserved for cuinterpose", container.Name)
			}
		}
	}
	return nil
}

// cuInterposeTarget returns the target container after checking that it can start
// under the launcher and does not already use the delivery path.
func cuInterposeTarget(spec *corev1.PodSpec, name string) (*corev1.Container, error) {
	index := -1
	for i := range spec.Containers {
		if spec.Containers[i].Name == name {
			index = i
			break
		}
	}
	if index == -1 {
		return nil, fmt.Errorf("cuinterpose target container %q not found", name)
	}
	target := &spec.Containers[index]
	if len(target.Command) == 0 || target.Command[0] == "" {
		return nil, fmt.Errorf("container %q: cuinterpose requires container.command", name)
	}
	for _, mount := range target.VolumeMounts {
		mountPath := path.Clean(mount.MountPath)
		if mount.Name == cuInterposeVolumeName || mountPath == podcontract.CuInterposeMountPath ||
			strings.HasPrefix(mountPath, podcontract.CuInterposeMountPath+"/") {
			return nil, fmt.Errorf("container %q: mount %q at %q conflicts with cuinterpose delivery", name, mount.Name, mount.MountPath)
		}
	}
	return target, nil
}

// cuInterposeInstaller copies the payload as a nonroot user, so Pods with runAsNonRoot
// can start it too. Equal requests and limits keep a Guaranteed Pod's QoS class.
func cuInterposeInstaller(podSecurity *corev1.PodSecurityContext, installer operatortypes.CuInterposeContainerConfiguration) corev1.Container {
	uid := cuInterposeInstallerUID
	if podSecurity != nil && podSecurity.RunAsUser != nil && *podSecurity.RunAsUser > 0 {
		uid = *podSecurity.RunAsUser
	}
	resources := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")}
	return corev1.Container{
		Name:            cuInterposeInitContainerName,
		Image:           installer.Image,
		ImagePullPolicy: installer.PullPolicy,
		Command:         []string{"/bin/cp"},
		Args: []string{
			"--preserve=mode", "--",
			cuInterposeImageFrontend, cuInterposeImageCore, cuInterposeImageLauncher,
			podcontract.CuInterposeMountPath + "/",
		},
		VolumeMounts: []corev1.VolumeMount{{Name: cuInterposeVolumeName, MountPath: podcontract.CuInterposeMountPath}},
		Resources:    corev1.ResourceRequirements{Requests: resources, Limits: resources.DeepCopy()},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To(uid),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
}
