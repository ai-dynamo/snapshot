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

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// CheckWritePath refuses a file action that would create one of the files
// Snapshot itself writes in the container's snapshot-control volume. Any other
// path is the application's to choose: the action runs as the container's
// user, so it can only write where that user could.
//
// Reads need no check, so a probe may watch ready-for-snapshot.
func CheckWritePath(pod *corev1.Pod, container, filePath string) error {
	c := findContainer(pod, container)
	if c == nil {
		return fmt.Errorf("container %q is not in Pod %s/%s", container, pod.Namespace, pod.Name)
	}
	if !podcontract.IsReservedControlFile(path.Base(filePath)) {
		return nil
	}
	for _, m := range c.VolumeMounts {
		if m.Name == podcontract.SnapshotControlVolumeName && path.Dir(filePath) == path.Clean(m.MountPath) {
			return fmt.Errorf("path %s is a file Snapshot writes itself; use another name", filePath)
		}
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
