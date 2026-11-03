# API Reference

Use this page to look up the Snapshot API. It lists the three custom resources,
`PodSnapshot`, `PodSnapshotContent` and `SnapshotJob`, and the annotations,
labels, pod conditions and control volume that work with them. All custom
resources are in API group `nvidia.com`, version `v1alpha1`.

To see how these fit together, read [Core Concepts](../overview/core-concepts.md).
To use them in a checkpoint and restore, follow
[Using Snapshot](../using-snapshot/README.md). For chart settings, see
[Helm Chart Values](helm-values.md).

## Custom Resources

| Kind                 | Scope      | Short name       | Created by                                   |
|----------------------|------------|------------------|----------------------------------------------|
| `PodSnapshot`        | Namespaced | `podsnap`        | The caller (or a controller / `SnapshotJob`) |
| `PodSnapshotContent` | Cluster    | `podsnapcontent` | The operator only                            |
| `SnapshotJob`        | Namespaced | `snapjob`        | The caller                                   |

Each resource's `spec` is immutable after creation.

### PodSnapshot

The namespaced binding for a captured container checkpoint; consumed by restore.

`spec`:

| Field                      | Type     | Required | Description                                                                               |
|----------------------------|----------|----------|-------------------------------------------------------------------------------------------|
| `source.podRef.name`       | string   | yes      | Name of the source pod, in the same namespace.                                            |
| `source.podRef.uid`        | string   | no       | UID of the source pod, so the agent dumps that exact pod and not a same-named recreation. |
| `source.podRef.containers` | []string | yes      | Container(s) to checkpoint. Exactly one in `v1alpha1`; each must be a DNS label.          |

`status`:

| Field                      | Type        | Description                                                                                                              |
|----------------------------|-------------|--------------------------------------------------------------------------------------------------------------------------|
| `boundSnapshotContentName` | string      | Name of the bound cluster-scoped `PodSnapshotContent`; unset until the agent binds it.                                   |
| `conditions`               | []Condition | `Ready` (capture and binding complete, artifact usable for restore) and `Failed` (capture or binding failed terminally). |

### PodSnapshotContent

The cluster-scoped artifact-of-record for a checkpoint. The operator creates it
when it binds a `PodSnapshot`; callers never create it.

`spec`:

| Field                   | Type         | Required | Description                                                                                                                                                                                                      |
|-------------------------|--------------|----------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `snapshotRef.namespace` | string       | yes      | Namespace of the bound `PodSnapshot`.                                                                                                                                                                            |
| `snapshotRef.name`      | string       | yes      | Name of the bound `PodSnapshot`.                                                                                                                                                                                 |
| `snapshotRef.uid`       | string       | no       | UID recorded at binding time, to detect a delete-and-recreate.                                                                                                                                                   |
| `source.podRef`         | PodReference | yes      | The pod to dump (`name` / `uid` / `containers`).                                                                                                                                                                 |
| `source.nodeName`       | string       | yes      | Node the source pod runs on; selects the node agent that performs the dump.                                                                                                                                      |
| `storage.storeID`       | string       | no       | Backend-neutral store binding, immutable with the rest of `spec`. When `storage` is present, the ID must be `store-v1-` followed by 64 lowercase hexadecimal characters. Omitted by legacy filesystem producers. |

`status`:

| Field                                           | Type                         | Description                                                                                                                                                                                                                                |
|-------------------------------------------------|------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `conditions`                                    | []Condition                  | `Ready` (artifact captured and usable for restore) and `Failed` (capture failed terminally).                                                                                                                                               |
| `source`                                        | CheckpointSource             | What the checkpoint was captured on, written with `Ready`. Informational only: the restore compatibility gates compare the artifact's manifest, not this block. Every field below is optional and absent when the value could not be read. |
| `source.node.name`                              | string                       | Node the source pod ran on.                                                                                                                                                                                                                |
| `source.node.architecture`                      | string                       | Node CPU architecture, as `GOARCH` spells it.                                                                                                                                                                                              |
| `source.node.kernelVersion`                     | string                       | Node kernel release.                                                                                                                                                                                                                       |
| `source.node.cpuFeatures`                       | []string                     | Instruction-set flags from the node's `/proc/cpuinfo` that every processor reported, such as `avx512f` on x86_64 or `sve2` on arm64, sorted.                                                                                               |
| `source.pod.image`                              | string                       | Container image reference the capture ran.                                                                                                                                                                                                 |
| `source.pod.imageDigest`                        | string                       | Identifies which build of the image ran, which a mutable tag does not.                                                                                                                                                                     |
| `source.pod.memory`                             | string                       | Container memory limit; absent if it had none.                                                                                                                                                                                             |
| `source.pod.cpu`                                | string                       | Container CPU limit; absent if it had none.                                                                                                                                                                                                |
| `source.devices.nvidia.driverVersion`           | string                       | NVIDIA driver the capture ran against.                                                                                                                                                                                                     |
| `source.devices.nvidia.instances[].productName` | string                       | GPU model as `nvidia-smi` reports it, one entry per GPU the captured container could see.                                                                                                                                                  |
| `storage.artifacts`                             | []PublishedContainerArtifact | Optional confirmed publications, keyed by `containerName`. Requires `spec.storage`; container names must be captured targets. Does not replace Ready/Failed conditions.                                                                    |
| `storage.artifacts[].containerName`             | string                       | Required container name; entries must be unique.                                                                                                                                                                                           |
| `storage.artifacts[].artifactHandle`            | string                       | Required opaque publication locator within the bound store, up to 4096 characters. Callers must not interpret it as a filesystem path.                                                                                                     |

### SnapshotJob

Runs a checkpoint-ready workload pod and captures it into a `PodSnapshot` in one
declarative object.

`spec`:

| Field                                  | Type            | Required | Default    | Description                                                                                                                                                                             |
|----------------------------------------|-----------------|----------|------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `podTemplate`                          | PodTemplateSpec | yes      | —          | The workload to run and capture. The controller injects the snapshot contract (control volume, readiness probe, seccomp); image, command, GPU resources, and sidecars are the caller's. |
| `podSnapshotTemplate.targetContainers` | []string        | no       | `["main"]` | Container(s) to checkpoint. Exactly one in `v1alpha1`; each must name a container present in `podTemplate`.                                                                             |
| `activeDeadlineSeconds`                | int64           | no       | `3600`     | Total time allowed for scheduling, quiesce, and dump; applied to the batch/v1 Job.                                                                                                      |
| `nonTargetArtifacts.containers`        | []string        | no       | —          | Non-target containers that write artifacts to the shared checkpoint PVC. Must be nonempty if `nonTargetArtifacts` is set.                                                               |
| `nonTargetArtifacts.onFailure`         | string          | no       | `Retain`   | `Retain` keeps a failed attempt's artifacts. `Cleanup` lets the operator delete them once all writers have stopped.                                                                     |

`podSnapshotTemplate` itself is required; its only field, `targetContainers`,
defaults. The object's `metadata.name` must be at most 63 characters (it is used
as a label value).

`status`:

| Field             | Type        | Description                                                     |
|-------------------|-------------|-----------------------------------------------------------------|
| `podSnapshotName` | string      | Name of the produced `PodSnapshot` (empty means never created). |
| `podSnapshotUID`  | string      | UID of the produced `PodSnapshot`.                              |
| `sourceJobUID`    | string      | UID of the source batch/v1 Job.                                 |
| `startedAt`       | time        | When the source pod was first observed Ready.                   |
| `completedAt`     | time        | When a terminal condition was set.                              |
| `conditions`      | []Condition | See below.                                                      |

`SnapshotJob` conditions (all four are always present; read `reason` and `message` for detail):

| Type        | True when                                                                       |
|-------------|---------------------------------------------------------------------------------|
| `Running`   | The source pod is running and ready.                                            |
| `Captured`  | The CRIU dump of the target container is complete (the `PodSnapshot` is Ready). |
| `Completed` | The checkpoint is durable and the source Job has finished.                      |
| `Failed`    | A terminal failure occurred.                                                    |

## Restore

The caller sets these annotations on the new pod to trigger a restore:

| Annotation                              | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
|-----------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `nvidia.com/restore-from`               | Names the `PodSnapshot`, in the pod's namespace, to restore into the pod.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `nvidia.com/restore-container-map`      | Optional. Comma-separated `source=destination` pairs mapping the single captured container to one or more restore containers. When absent, the captured container name is the destination.                                                                                                                                                                                                                                                                                                                                                                       |
| `nvidia.com/snapshot-skip-compat-check` | Optional. Set to `"true"` to attempt the restore without the compatibility checks, which otherwise refuse a checkpoint this node cannot run. The value must parse as a boolean, so any other spelling, including `"yes"`, leaves the checks in place. Treat it as a debugging escape hatch: a restore that should have been refused instead fails somewhere inside CRIU. Adding it to a pod whose restore was already refused reopens that restore in place, so this is the one case that needs no new pod. A restore that was attempted and failed still does. |

Snapshot then reports restore progress with a pod status condition, written by
the node agent and not set by the caller:

| Pod condition         | Description                                                                                                                                                         |
|-----------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `nvidia.com/Restored` | Added to the pod by the node agent once a restore is under way; becomes `True` when the restore completes. Observe it alongside pod readiness to confirm a restore. |

## Labels

Snapshot sets and consumes these labels itself; callers do not set them. They are
listed for selection and debugging.

| Label                                  | Applied to                      | Purpose                                                                                                        |
|----------------------------------------|---------------------------------|----------------------------------------------------------------------------------------------------------------|
| `nvidia.com/snapshot-capture-eligible` | Source pod                      | Added by the agent's pre-bind gate after the source pod passes validation; the capture informer selects on it. |
| `nvidia.com/snapshot-node`             | `PodSnapshotContent`            | Mirrors `spec.source.nodeName` so each node agent can select its own work.                                     |
| `nvidia.com/snapshot-job`              | `PodSnapshot`                   | Maps a produced `PodSnapshot` back to the `SnapshotJob` that created it.                                       |
| `nvidia.com/snapshot-job-uid`          | `SnapshotJob`-created resources | Binds them to one `SnapshotJob` incarnation, since names can be reused.                                        |

## The Snapshot-Control Volume

Checkpoint and restore are coordinated through a per-pod `emptyDir` that the
workload and the node agent share.

| Item        | Value                  | Description                                                                                         |
|-------------|------------------------|-----------------------------------------------------------------------------------------------------|
| Volume name | `snapshot-control`     | Per-pod `emptyDir`. With multiple target containers, each mounts it with `subPath=<containerName>`. |
| Mount path  | `/snapshot-control`    | Where the workload sees the control directory.                                                      |
| Environment | `SNAPSHOT_CONTROL_DIR` | Exposes the mount path to the workload (legacy name: `DYN_SNAPSHOT_CONTROL_DIR`).                   |

Sentinel files inside the volume:

| File                  | Written by | Meaning                                                                                     |
|-----------------------|------------|---------------------------------------------------------------------------------------------|
| `ready-for-snapshot`  | Workload   | The model is loaded and it is safe to checkpoint. The source readiness probe gates on this. |
| `restore-complete`    | Node agent | The restore finished and the workload may resume.                                           |
| `cuda-checkpoint-job` | Node agent | The persisted CUDA checkpoint job file.                                                     |

A checkpoint always terminates the source process, so there is no
`snapshot-complete` sentinel. Checkpointing also requires a seccomp profile that
blocks io_uring (which CRIU cannot checkpoint); the chart installs it at
`profiles/block-iouring.json`.

## Advanced: Non-Target Artifacts

A non-target container can write output to the shared checkpoint PVC. Declare it in `spec.nonTargetArtifacts.containers` (regular containers only, excluding capture targets). Do not combine this field with the legacy `nvidia.com/snapshot-non-target-artifact-containers` annotation.

Snapshot injects `SNAPSHOT_JOB_UID` and `SNAPSHOT_NON_TARGET_ARTIFACT_SUBDIR` (`non-target-artifacts/<SnapshotJob UID>`, relative to the PVC root). Supply the PVC mount, write beneath that directory, and finish before the container exits. It must be writable by the container and removable by the operator. Restore consumers find the attempt through the `PodSnapshot`'s `nvidia.com/snapshot-job-uid` label.

On failure, `Retain` (the default) keeps the output. `Cleanup` deletes it only when the `SnapshotJob` is `Failed` and all source Pod containers are confirmed stopped. If proof cannot arrive, such as after Pod removal, Snapshot retains the directory and emits a `NonTargetArtifactsRetained` warning once per attempt per operator process. Verify writer exit before manual removal. Successful output is caller-managed. This applies only to the shared PVC, not S3 or capture artifacts.
