// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package lifecyclehooks runs the file and HTTP actions a container's settings
// ask for at points in its checkpoint and restore lifecycle: the checkpoint
// readiness probe and the post-restore action. It only performs the actions;
// when to run them, how often, and what a result means is the caller's.
package lifecyclehooks

import (
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// CheckPath refuses a file action whose path the agent must not touch. The
// path must be on the container's own filesystem or on an emptyDir volume:
// other volumes may be shared with other Pods or with the node, and the agent
// acts as root. A write may also not name a file Snapshot itself writes in the
// snapshot-control volume; a read may, so a probe can watch ready-for-snapshot.
//
// The check reads the Pod spec only. The file action itself resolves the path
// inside the container without following symlinks, so a link can't lead it
// from an allowed place to a refused one.
func CheckPath(pod *corev1.Pod, container, filePath string, write bool) error {
	if err := validatePath(filePath); err != nil {
		return err
	}
	c := findContainer(pod, container)
	if c == nil {
		return fmt.Errorf("container %q is not in Pod %s/%s", container, pod.Namespace, pod.Name)
	}
	mount := mountFor(c, filePath)
	if mount == nil {
		return nil // the container's own filesystem
	}
	volume := findVolume(pod, mount.Name)
	if volume == nil {
		return fmt.Errorf("path %s is on volume %q, which is not in the Pod spec", filePath, mount.Name)
	}
	if volume.EmptyDir == nil {
		return fmt.Errorf("path %s is on volume %q, which is not an emptyDir; "+
			"use a path on the container's own filesystem or an emptyDir volume", filePath, mount.Name)
	}
	if write && volume.Name == podcontract.SnapshotControlVolumeName &&
		path.Dir(filePath) == path.Clean(mount.MountPath) && podcontract.IsReservedControlFile(path.Base(filePath)) {
		return fmt.Errorf("path %s is a file Snapshot writes itself; use another name", filePath)
	}
	return nil
}

// validatePath repeats the API's rules, so a path that reached the agent some
// other way can't escape: absolute, clean, and not the root.
func validatePath(filePath string) error {
	if !strings.HasPrefix(filePath, "/") || filePath == "/" || path.Clean(filePath) != filePath {
		return fmt.Errorf("path %q must be absolute and clean, and name a file", filePath)
	}
	return nil
}

func findContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// mountFor returns the volume mount filePath is on: the one with the longest
// mount path that contains it, as the kernel resolves nested mounts.
func mountFor(c *corev1.Container, filePath string) *corev1.VolumeMount {
	var best *corev1.VolumeMount
	for i := range c.VolumeMounts {
		m := &c.VolumeMounts[i]
		mountPath := path.Clean(m.MountPath)
		if mountPath != "/" && filePath != mountPath && !strings.HasPrefix(filePath, mountPath+"/") {
			continue
		}
		if best == nil || len(mountPath) > len(path.Clean(best.MountPath)) {
			best = m
		}
	}
	return best
}

func findVolume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}
