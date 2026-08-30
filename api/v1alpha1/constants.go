// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Snapshot control-plane contract: the labels, annotations, and control-volume
// vocabulary the operator stamps and the node agent reads (and that external
// consumers such as the restore webhook and the workload depend on). This is the
// single versioned home for these constants so both sides pin them from the api
// module.
const (
	// CaptureEligibleLabel is the gate-applied promotion label: the node agent's pre-bind gate
	// adds it only after the source pod passes validation. The agent's source-pod capture
	// informer keys on it so only gate-validated pods drive the capture path.
	CaptureEligibleLabel = "nvidia.com/snapshot-capture-eligible"

	// SnapshotNodeLabel mirrors PodSnapshotContent.spec.source.nodeName onto the
	// object so the per-node agent's cache can label-select work for its node.
	SnapshotNodeLabel = "nvidia.com/snapshot-node"

	// RestoreFromAnnotation names the PodSnapshot to restore in the pod's
	// namespace.
	RestoreFromAnnotation = "nvidia.com/restore-from"

	// RestoreContainerMapAnnotation optionally maps the single captured source
	// container to one or more restore destinations. Its value is a comma-separated
	// list of source=destination pairs. When absent, restore uses the captured
	// container name as the destination.
	RestoreContainerMapAnnotation = "nvidia.com/restore-container-map"

	// RestoredCondition is the Pod status condition owned by the node agent.
	RestoredCondition = "nvidia.com/Restored"

	CheckpointVolumeName           = "checkpoint-storage"
	DefaultSeccompLocalhostProfile = "profiles/block-iouring.json"
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

	// SnapshotControlDirEnv is the canonical environment variable exposing the
	// control mount path to the workload.
	SnapshotControlDirEnv = "SNAPSHOT_CONTROL_DIR"

	// LegacySnapshotControlDirEnv is the environment variable exposing the
	// control mount path to the workload. EnsureControlVolume injects both
	// during the migration window so existing workload images (which read
	// this name) keep working while new images can move to
	// SnapshotControlDirEnv.
	//
	// Deprecated: use SnapshotControlDirEnv instead. Remove once no workload
	// image depends on this name.
	LegacySnapshotControlDirEnv = "DYN_SNAPSHOT_CONTROL_DIR"

	// SnapshotCompleteFile named the sentinel the agent used to release a
	// checkpointed workload when leave-running dumps existed. A checkpoint now
	// always terminates the source process, so the agent no longer writes it;
	// a workload polling for this file is killed instead of released.
	//
	// Deprecated: no longer written. Remove once no workload image waits on it.
	SnapshotCompleteFile = "snapshot-complete"

	// RestoreCompleteFile is written by the snapshot agent inside the
	// control volume when a restore has completed and the workload may
	// resume.
	RestoreCompleteFile = "restore-complete"

	// ReadyForSnapshotFile is written by the workload inside the control
	// volume when the model is loaded and the workload is ready for a
	// checkpoint. Observed by the source job's kubelet readiness probe
	// on the worker container.
	ReadyForSnapshotFile = "ready-for-snapshot"

	// CUDAJobFileName is the stable name the cuda-checkpoint-helper launch-job
	// wrapper persists the CUDA checkpoint job file under, inside the control
	// volume. The stable location survives past the transient path the CUDA
	// driver reports via CUDA_CHECKPOINT_JOB_FILE.
	CUDAJobFileName = "cuda-checkpoint-job"

	// CUDAJobFilePath is the full stable path to the persisted CUDA checkpoint
	// job file.
	CUDAJobFilePath = SnapshotControlMountPath + "/" + CUDAJobFileName
)
