// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
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

// CuInterposeLauncherPrefix starts a target command under the launcher. SnapshotJob adds
// it to opted-in sources, and ordinary Pods must add it themselves.
func CuInterposeLauncherPrefix() []string {
	return []string{CuInterposeLauncherPath, "--library", CuInterposeLibraryPath, "--"}
}

// ValidateCuInterposeLauncher checks that an opted-in container's command starts with
// the launcher prefix and that a workload follows in command or args.
func ValidateCuInterposeLauncher(container *corev1.Container) error {
	prefix := CuInterposeLauncherPrefix()
	if len(container.Command) < len(prefix) || !slices.Equal(container.Command[:len(prefix)], prefix) ||
		len(container.Command)+len(container.Args) == len(prefix) {
		return fmt.Errorf("container %q requests cuinterpose, so its command must start with %q "+
			"followed by the workload in command or args", container.Name, strings.Join(prefix, " "))
	}
	return nil
}
