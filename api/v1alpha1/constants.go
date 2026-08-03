// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Snapshot control-plane contract: the labels, annotations, and control-volume
// vocabulary the operator stamps and the node agent reads (and that external
// consumers such as the restore webhook and the workload depend on). This is the
// single versioned home for these constants so both sides pin them from the api
// module.
const (
	CheckpointSourceLabel = "nvidia.com/snapshot-is-checkpoint-source"

	// CaptureEligibleLabel is the gate-applied promotion label. The operator stamps
	// CheckpointSourceLabel on the checkpoint Job pod at creation; the node agent's pre-bind gate adds
	// CaptureEligibleLabel only after the source pod passes validation. The source-pod capture
	// informer keys on CaptureEligibleLabel so only gate-validated pods drive the capture path.
	CaptureEligibleLabel = "nvidia.com/snapshot-capture-eligible"

	// Restore pods carry CheckpointIDLabel without CheckpointSourceLabel.
	CheckpointIDLabel  = "nvidia.com/snapshot-checkpoint-id"
	RestoreTargetLabel = "nvidia.com/snapshot-is-restore-target"

	CheckpointArtifactVersionAnnotation = "nvidia.com/snapshot-artifact-version"

	// SnapshotNodeLabel mirrors PodSnapshotContent.spec.source.nodeName onto the
	// object so the per-node agent's cache can label-select work for its node.
	SnapshotNodeLabel = "nvidia.com/snapshot-node"

	// Required comma-separated checkpoint/restore target container list.
	TargetContainersAnnotation = "nvidia.com/snapshot-target-containers"

	CheckpointStatusAnnotation = "nvidia.com/snapshot-checkpoint-status"

	// Full keys are nvidia.com/snapshot-restore-status.<containerName>.
	RestoreStatusAnnotationPrefix = "nvidia.com/snapshot-restore-status."

	// Full keys are nvidia.com/snapshot-restore-container-id.<containerName>.
	RestoreContainerIDAnnotationPrefix = "nvidia.com/snapshot-restore-container-id."

	// Legacy unscoped restore status keys, cleared when stamping fresh metadata.
	RestoreStatusAnnotation      = "nvidia.com/snapshot-restore-status"
	RestoreContainerIDAnnotation = "nvidia.com/snapshot-restore-container-id"

	CheckpointStorageTypeAnnotation     = "nvidia.com/snapshot-storage-type"
	CheckpointStorageBasePathAnnotation = "nvidia.com/snapshot-storage-base-path"
	PageBrokerAnnotation                = "nvidia.com/snapshot-pagebroker"
	CheckpointVolumeName                = "checkpoint-storage"
	DefaultCheckpointArtifactVersion    = "1"
	DefaultCheckpointJobTTLSeconds      = int32(300) // TODO: dead code — remove once no longer synced from Dynamo
	DefaultSeccompLocalhostProfile      = "profiles/block-iouring.json"
	StorageTypePVC                      = "pvc"

	CheckpointStatusCompleted = "completed" // TODO: dead code — remove once no longer synced from Dynamo
	CheckpointStatusFailed    = "failed"    // TODO: dead code — remove once no longer synced from Dynamo
	RestoreStatusInProgress   = "in_progress"
	RestoreStatusCompleted    = "completed"
	RestoreStatusFailed       = "failed"
)

// Control-volume contract: the per-pod emptyDir carrying checkpoint/restore
// lifecycle sentinels written by the snapshot agent and observed by the workload.
const (
	// SnapshotControlVolumeName is the per-pod emptyDir used to carry
	// checkpoint/restore lifecycle sentinels written by the snapshot agent
	// and observed by the workload. It replaces the SIGUSR1/SIGCONT signals
	// that previously required the workload to run as PID 1.
	//
	// When a pod targets multiple containers (e.g. failover engine-0 +
	// engine-1), each container mounts the emptyDir with
	// subPath=<containerName>, so sentinels are isolated per-container on
	// disk while each container still sees them at SnapshotControlMountPath.
	SnapshotControlVolumeName = "snapshot-control"

	// SnapshotControlMountPath is where the control volume is mounted inside
	// the workload container.
	SnapshotControlMountPath = "/snapshot-control"

	// SnapshotControlDirEnv is the environment variable exposing the control
	// mount path to the workload.
	SnapshotControlDirEnv = "DYN_SNAPSHOT_CONTROL_DIR"

	// SnapshotCompleteFile is written by the snapshot agent inside the
	// control volume when a checkpoint has completed successfully.
	SnapshotCompleteFile = "snapshot-complete"

	// RestoreCompleteFile is written by the snapshot agent inside the
	// control volume when a restore has completed and the workload may
	// resume.
	RestoreCompleteFile = "restore-complete"

	// ReadyForSnapshotFile is written by the workload inside the control
	// volume when the model is loaded and the workload is ready for a
	// checkpoint. Observed by the checkpoint job's kubelet readiness probe
	// on the worker container.
	ReadyForSnapshotFile = "ready-for-snapshot"
)
