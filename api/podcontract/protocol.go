// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Snapshot workload Pod annotations and control protocol.
const (
	// RestoreFromAnnotation names the PodSnapshot to restore in the Pod's
	// namespace.
	RestoreFromAnnotation = "nvidia.com/restore-from"

	// RestoreContainerMapAnnotation optionally maps the single captured source
	// container to one or more restore destinations. Its value is a
	// comma-separated list of source=destination pairs. When absent, restore
	// uses the captured container name as the destination.
	RestoreContainerMapAnnotation = "nvidia.com/restore-container-map"

	// DefaultSeccompLocalhostProfile is the kubelet-local profile installed by
	// the Snapshot Helm chart to block io_uring for CRIU.
	DefaultSeccompLocalhostProfile = "profiles/block-iouring.json"

	// SnapshotControlVolumeName is the per-Pod emptyDir used to carry capture and
	// restore lifecycle files between Snapshot and the workload.
	SnapshotControlVolumeName = "snapshot-control"

	// SnapshotControlMountPath is where the control volume is mounted inside a
	// Snapshot-managed workload container.
	SnapshotControlMountPath = "/snapshot-control"

	// SnapshotControlDirEnv is the canonical environment variable exposing the
	// control mount path to the workload.
	SnapshotControlDirEnv = "SNAPSHOT_CONTROL_DIR"

	// LegacySnapshotControlDirEnv is the deprecated environment variable
	// exposing the control mount path to the workload.
	//
	// Deprecated: use SnapshotControlDirEnv instead.
	LegacySnapshotControlDirEnv = "DYN_SNAPSHOT_CONTROL_DIR"

	// ReadyForSnapshotFile is written by the workload when the model is loaded
	// and the workload is ready for capture. The source Pod's kubelet readiness
	// probe observes it through the control volume.
	ReadyForSnapshotFile = "ready-for-snapshot"

	// CUDAJobFileName is the stable name under which the
	// cuda-checkpoint-helper launch-job wrapper persists the CUDA checkpoint job
	// file inside the control volume.
	CUDAJobFileName = "cuda-checkpoint-job"

	// CUDAJobFilePath is the full stable path to the persisted CUDA checkpoint
	// job file.
	CUDAJobFilePath = SnapshotControlMountPath + "/" + CUDAJobFileName

	// RestoreStandbyModeEnv asks standby-aware workload entrypoints to remain
	// inert until Snapshot replaces them with restored processes. Snapshot
	// publishes the name so producers and workloads agree on it, but does not
	// inject this workload-specific setting.
	RestoreStandbyModeEnv = "SNAPSHOT_RESTORE_STANDBY"

	// RestoreCompleteFile is written by the Snapshot agent when restore has
	// completed and the workload may resume.
	RestoreCompleteFile = "restore-complete"
)

// ContainerMapping maps the one captured source container to a restore
// destination in the target Pod.
type ContainerMapping struct {
	Source      string
	Destination string
}

// GetRestoreFromSnapshotName returns the same-namespace PodSnapshot named by
// the restore-from annotation.
func GetRestoreFromSnapshotName(annotations map[string]string) (string, error) {
	return validateRestoreFromSnapshotName(annotations[RestoreFromAnnotation])
}

func validateRestoreFromSnapshotName(value string) (string, error) {
	snapshotName := strings.TrimSpace(value)
	if snapshotName == "" {
		return "", fmt.Errorf("%s must name a PodSnapshot", RestoreFromAnnotation)
	}
	if errs := validation.IsDNS1123Subdomain(snapshotName); len(errs) != 0 {
		return "", fmt.Errorf(
			"%s value %q is not a valid PodSnapshot name: %s",
			RestoreFromAnnotation,
			snapshotName,
			strings.Join(errs, "; "),
		)
	}
	return snapshotName, nil
}

// ContainerMappingsFromAnnotations parses the optional flat restore mapping.
// Absence keeps the existing same-name restore behavior.
func ContainerMappingsFromAnnotations(
	annotations map[string]string,
	capturedSource string,
) ([]ContainerMapping, error) {
	raw, ok := annotations[RestoreContainerMapAnnotation]
	if !ok {
		capturedSource = strings.TrimSpace(capturedSource)
		return []ContainerMapping{{Source: capturedSource, Destination: capturedSource}}, nil
	}
	parts := strings.Split(strings.TrimSpace(raw), ",")
	mappings := make([]ContainerMapping, 0, len(parts))
	for _, part := range parts {
		pair := strings.Split(part, "=")
		if len(pair) != 2 {
			return nil, fmt.Errorf(
				"invalid %s entry %q: expected source=destination",
				RestoreContainerMapAnnotation,
				strings.TrimSpace(part),
			)
		}
		mappings = append(mappings, ContainerMapping{
			Source:      strings.TrimSpace(pair[0]),
			Destination: strings.TrimSpace(pair[1]),
		})
	}
	return mappings, nil
}

// ValidateContainerMappings enforces the one-source-to-many-destinations
// contract after parsing.
func ValidateContainerMappings(mappings []ContainerMapping, capturedSource string) error {
	capturedSource = strings.TrimSpace(capturedSource)
	if errs := validation.IsDNS1123Label(capturedSource); len(errs) != 0 {
		return fmt.Errorf("captured source container %q is invalid: %s", capturedSource, strings.Join(errs, "; "))
	}
	if len(mappings) == 0 {
		return fmt.Errorf("restore container mapping must contain at least one destination")
	}
	destinations := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		source := mapping.Source
		destination := mapping.Destination
		if errs := validation.IsDNS1123Label(source); len(errs) != 0 {
			return fmt.Errorf("invalid restore source container %q: %s", source, strings.Join(errs, "; "))
		}
		if errs := validation.IsDNS1123Label(destination); len(errs) != 0 {
			return fmt.Errorf("invalid restore destination container %q: %s", destination, strings.Join(errs, "; "))
		}
		if source != capturedSource {
			return fmt.Errorf("restore source container %q does not match captured container %q", source, capturedSource)
		}
		if _, duplicate := destinations[destination]; duplicate {
			return fmt.Errorf("duplicate restore destination container %q", destination)
		}
		destinations[destination] = struct{}{}
	}
	return nil
}
