// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
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
	// uses the captured container name as the destination. On a group target,
	// only the operator writes it, when it claims the Pod, and it always holds
	// exactly one pair, even for a restore into the container of the same name.
	RestoreContainerMapAnnotation = "nvidia.com/restore-container-map"

	// RestoreGroupSchedulingGate holds a group restore Pod until the operator
	// claims it. The owner sets it when it creates the Pod, through
	// BuildGroupTarget, typically in a template that every replica shares or a
	// Pod-creation webhook. Only the operator removes it, in the same patch
	// that writes RestoreGroupUIDAnnotation and RestoreContainerMapAnnotation,
	// so a gated Pod has never run, and a Pod created without it is never a
	// group target.
	RestoreGroupSchedulingGate = "nvidia.com/restore-group"

	// RestoreGroupUIDAnnotation holds the UID of the PodGroupRestore that
	// claimed the Pod. Only the operator writes it, together with the target's
	// single-pair RestoreContainerMapAnnotation, in the patch conditional on
	// the Pod's resourceVersion that removes RestoreGroupSchedulingGate, so
	// both are on the Pod before its sandbox exists and the NRI plugin reads
	// them without an API call. A Pod is claimed once and never rebound, so
	// the binding names the only attempt that may act or report on the Pod,
	// and the agent finds its attempt through it. ClaimGroupTarget writes it,
	// and RestoreGroupClaimOf reads it.
	RestoreGroupUIDAnnotation = "nvidia.com/restore-group-uid"

	// SkipCompatCheckAnnotation disables compatibility checks for one restore.
	SkipCompatCheckAnnotation = "nvidia.com/snapshot-skip-compat-check"

	// CuInterposeAnnotation enables multi-GPU support, which runs checkpoint
	// targets under the CUDA interposer.
	CuInterposeAnnotation = "nvidia.com/snapshot-multi-gpu"

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

	// CoordinationDir is the directory of the control volume reserved for the
	// rendezvous endpoints of a group member's communication libraries.
	CoordinationDir = "coordination"

	// RestoreStandbyModeEnv asked standby-aware workload entrypoints to remain
	// inert until Snapshot replaced them with restored processes. Snapshot
	// published the name but never injected it.
	//
	// Deprecated: Snapshot now replaces a restore container's command with
	// RestoreProxyPath, so the entrypoint never runs and nothing needs to stay
	// inert. Restore Pods can keep the source's command.
	RestoreStandbyModeEnv = "SNAPSHOT_RESTORE_STANDBY"

	// LegacyRestoreStandbyModeEnv is the previous name of RestoreStandbyModeEnv,
	// kept for existing workload integrations that still read it. Snapshot
	// publishes the name but does not inject it.
	//
	// Deprecated: see RestoreStandbyModeEnv. This alias is removed in the next
	// release.
	LegacyRestoreStandbyModeEnv = "DYN_SNAPSHOT_RESTORE_STANDBY"

	// RestoreCompleteFile is written by the Snapshot agent when restore has
	// completed and the workload may resume. Its contents are key=value lines
	// built by FormatRestoreComplete; workloads that only gate on the file's
	// existence may ignore them.
	RestoreCompleteFile = "restore-complete"

	// RestoreFailedFile is written by the Snapshot agent before it kills a
	// destination container whose restore failed. The failing container never
	// acts on it; a later container in the same Pod reads it as proof that the
	// Pod's one restore was already used.
	RestoreFailedFile = "restore-failed"

	// RestoreCompletePIDKey names the restored process's PID, relative to the
	// destination container's PID namespace, in RestoreCompleteFile.
	RestoreCompletePIDKey = "pid"

	// RestoreCompleteAttemptKey names, in RestoreCompleteFile, the UID of the
	// PodGroupRestore that released a group target. A standalone restore omits it.
	RestoreCompleteAttemptKey = "attempt"

	// RestoreProxyBinaryName is the PID 1 process Snapshot runs in restore
	// destination containers. It waits for RestoreCompleteFile, then follows
	// the restored process and exits with its status.
	RestoreProxyBinaryName = "snapshot-restore-proxy"

	// RestoreProxyPath is the proxy as the container sees it, in the control
	// volume. It is the whole command of a restore destination container.
	RestoreProxyPath = SnapshotControlMountPath + "/" + RestoreProxyBinaryName
)

// IsReservedControlFile reports whether name is a file that Snapshot or its
// helpers write in the snapshot-control volume. A workload's post-restore file
// action must not name one: writing it would act as Snapshot's own signal.
func IsReservedControlFile(name string) bool {
	switch name {
	case RestoreCompleteFile, RestoreFailedFile, RestoreProxyBinaryName, CUDAJobFileName:
		return true
	}
	return false
}

// Exit codes reserved by the restore proxy. Every other code is the restored
// workload's own, or 128 plus the signal number that killed it.
const (
	// RestoreNotRepeatableExitCode means this Pod's one restore is spent: the
	// proxy found RestoreCompleteFile or RestoreFailedFile at start, left by an
	// earlier container, or RestoreFailedFile appeared while it waited.
	RestoreNotRepeatableExitCode = 112

	// RestoreProxyErrorExitCode means the proxy could not follow the restored
	// process, for example because RestoreCompleteFile was malformed or the
	// restored PID was not its child.
	RestoreProxyErrorExitCode = 113
)

// RestoreComplete is the content of RestoreCompleteFile.
type RestoreComplete struct {
	// PID is the restored process, relative to the destination container's
	// PID namespace.
	PID int

	// Attempt is the UID of the PodGroupRestore that released a group target, and
	// empty for a standalone restore.
	Attempt types.UID
}

// FormatRestoreComplete renders the content of RestoreCompleteFile. The
// attempt key is written only for a group target, so a standalone restore's
// file is unchanged.
func FormatRestoreComplete(rc RestoreComplete) []byte {
	content := RestoreCompletePIDKey + "=" + strconv.Itoa(rc.PID) + "\n"
	if rc.Attempt != "" {
		content += RestoreCompleteAttemptKey + "=" + string(rc.Attempt) + "\n"
	}
	return []byte(content)
}

// ParseRestoreComplete reads the content of RestoreCompleteFile. Unknown keys
// are ignored so the agent can add fields without breaking older readers.
func ParseRestoreComplete(data []byte) (RestoreComplete, error) {
	var rc RestoreComplete
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return RestoreComplete{}, fmt.Errorf("invalid %s line %q: expected key=value", RestoreCompleteFile, line)
		}
		switch key {
		case RestoreCompletePIDKey:
			if rc.PID != 0 {
				return RestoreComplete{}, fmt.Errorf("duplicate %s key %q", RestoreCompleteFile, key)
			}
			pid, err := strconv.Atoi(value)
			if err != nil || pid <= 0 {
				return RestoreComplete{}, fmt.Errorf("invalid %s %s %q", RestoreCompleteFile, key, value)
			}
			rc.PID = pid
		case RestoreCompleteAttemptKey:
			if rc.Attempt != "" {
				return RestoreComplete{}, fmt.Errorf("duplicate %s key %q", RestoreCompleteFile, key)
			}
			if value == "" {
				return RestoreComplete{}, fmt.Errorf("invalid %s %s %q", RestoreCompleteFile, key, value)
			}
			rc.Attempt = types.UID(value)
		}
	}
	if rc.PID == 0 {
		return RestoreComplete{}, fmt.Errorf("%s has no %s", RestoreCompleteFile, RestoreCompletePIDKey)
	}
	return rc, nil
}

// ContainerMapping maps the one captured source container to a restore
// destination in the target Pod.
type ContainerMapping struct {
	Source      string
	Destination string
}

// String returns the mapping as source=destination, one pair of
// RestoreContainerMapAnnotation's format.
func (m ContainerMapping) String() string {
	return m.Source + "=" + m.Destination
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
		if err := validateContainerMappingNames(mapping); err != nil {
			return err
		}
		if mapping.Source != capturedSource {
			return fmt.Errorf("restore source container %q does not match captured container %q",
				mapping.Source, capturedSource)
		}
		if _, duplicate := destinations[mapping.Destination]; duplicate {
			return fmt.Errorf("duplicate restore destination container %q", mapping.Destination)
		}
		destinations[mapping.Destination] = struct{}{}
	}
	return nil
}

// validateContainerMappingNames checks that both containers of a mapping have
// valid container names. It needs no snapshot, so a group target's claim can
// be checked without knowing which container the member checkpointed.
func validateContainerMappingNames(mapping ContainerMapping) error {
	if err := validateRestoreContainerName("source", mapping.Source); err != nil {
		return err
	}
	return validateRestoreContainerName("destination", mapping.Destination)
}

func validateRestoreContainerName(role, name string) error {
	if errs := validation.IsDNS1123Label(name); len(errs) != 0 {
		return fmt.Errorf("invalid restore %s container %q: %s", role, name, strings.Join(errs, "; "))
	}
	return nil
}

// SkipCompatCheckFromAnnotations reports whether compatibility checks are disabled.
func SkipCompatCheckFromAnnotations(annotations map[string]string) bool {
	skip, err := strconv.ParseBool(strings.TrimSpace(annotations[SkipCompatCheckAnnotation]))
	return err == nil && skip
}
