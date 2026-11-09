// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// CuInterposeMountPath is where an opted-in target container sees the
	// cuinterpose libraries and launcher. Restore mounts the agent's matching
	// copies, verified against the checkpoint's library hashes, at the same path.
	CuInterposeMountPath = "/tmp/snapshot-cuda"

	// CuInterposeLibraryPath is the frontend library the launcher preloads.
	CuInterposeLibraryPath = CuInterposeMountPath + "/libcuinterpose.so"

	// CuInterposeLauncherPath is the launcher that starts the workload with the
	// frontend library preloaded.
	CuInterposeLauncherPath = CuInterposeMountPath + "/cuinterpose-launch"
)

// ParseCuInterposeAnnotation accepts enabled or disabled after trimming whitespace.
// An absent annotation leaves support disabled; other present values are invalid.
func ParseCuInterposeAnnotation(annotations map[string]string) (bool, error) {
	value, present := annotations[CuInterposeAnnotation]
	if !present {
		return false, nil
	}
	switch strings.TrimSpace(value) {
	case "enabled":
		return true, nil
	case "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("annotation %s: expected enabled or disabled, got %q", CuInterposeAnnotation, value)
	}
}

// CuInterposeLauncherPrefix starts a target command under the launcher. The agent's
// NRI plugin adds it to the process of an opted-in container that it delivers to.
func CuInterposeLauncherPrefix() []string {
	return []string{CuInterposeLauncherPath, "--library", CuInterposeLibraryPath, "--"}
}

// HasCuInterposeLauncher reports whether argv starts with the launcher prefix and a
// workload follows it.
func HasCuInterposeLauncher(argv []string) bool {
	prefix := CuInterposeLauncherPrefix()
	return len(argv) > len(prefix) && slices.Equal(argv[:len(prefix)], prefix)
}

// IsCuInterposeDeliveryPath reports whether a container mount path is, or is inside,
// the cuinterpose delivery path. A container that mounts there delivers the
// libraries itself, so the agent leaves it alone.
func IsCuInterposeDeliveryPath(mountPath string) bool {
	clean := path.Clean(mountPath)
	return clean == CuInterposeMountPath || strings.HasPrefix(clean, CuInterposeMountPath+"/")
}

// ValidateCuInterposeTarget checks that an opted-in target container will start under
// the launcher. Either the agent delivers it, which needs the snapshot-control mount
// and no mount on the delivery path, or the container starts the launcher itself.
// Self-delivery needs command: with only args, the image's entrypoint runs first
// and may never start the launcher.
func ValidateCuInterposeTarget(container *corev1.Container) error {
	if len(container.Command) > 0 && HasCuInterposeLauncher(append(slices.Clone(container.Command), container.Args...)) {
		return nil
	}
	controlMount := false
	for _, mount := range container.VolumeMounts {
		if IsCuInterposeDeliveryPath(mount.MountPath) {
			return fmt.Errorf("container %q requests multi-GPU support and mounts %q at %q, "+
				"so its command must start with %q followed by the workload",
				container.Name, mount.Name, mount.MountPath, strings.Join(CuInterposeLauncherPrefix(), " "))
		}
		if path.Clean(mount.MountPath) == SnapshotControlMountPath {
			controlMount = true
		}
	}
	if !controlMount {
		return fmt.Errorf("container %q requests multi-GPU support but does not mount the %s volume at %s, "+
			"so the Snapshot agent cannot start it with multi-GPU support",
			container.Name, SnapshotControlVolumeName, SnapshotControlMountPath)
	}
	return nil
}
