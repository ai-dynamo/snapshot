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

The namespaced binding for one container's snapshot; consumed by restore.

`spec`:

| Field                      | Type     | Required | Description                                                                                                         |
|----------------------------|----------|----------|---------------------------------------------------------------------------------------------------------------------|
| `source.podRef.name`       | string   | yes      | Name of the source pod, in the same namespace.                                                                      |
| `source.podRef.uid`        | string   | no       | UID of the source pod, so the agent dumps that exact pod and not a same-named recreation.                           |
| `source.podRef.containers` | []string | yes      | Container(s) to checkpoint. Exactly one in `v1alpha1`; each must be a DNS label.                                    |
| `containers`               | []ContainerSnapshotConfig | no       | Settings for a checkpointed container, by `name`. At most one entry. See [Container settings](#container-settings). |

`status`:

| Field                      | Type        | Description                                                                                                              |
|----------------------------|-------------|--------------------------------------------------------------------------------------------------------------------------|
| `boundSnapshotContentName` | string      | Name of the bound cluster-scoped `PodSnapshotContent`; unset until the agent binds it.                                   |
| `conditions`               | []Condition | `Ready` (checkpoint and binding complete, snapshot usable for restore) and `Failed` (checkpoint or binding failed terminally). |

### PodSnapshotContent

The cluster-scoped record of one container's snapshot. The operator creates it
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
| `containers`            | []ContainerSnapshotConfig | no       | The `PodSnapshot`'s [container settings](#container-settings), with defaults filled in. Every restore of this checkpoint uses them, even after the `PodSnapshot` or `SnapshotJob` changes or is gone.            |

`status`:

| Field                                           | Type                         | Description                                                                                                                                                                                                                                |
|-------------------------------------------------|------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `conditions`                                    | []Condition                  | `Ready` (snapshot taken and usable for restore) and `Failed` (checkpoint failed terminally).                                                                                                                                               |
| `source`                                        | CheckpointSource             | Where the snapshot was taken, written with `Ready`. Informational only: the restore compatibility gates compare the snapshot's manifest, not this block. Every field below is optional and absent when the value could not be read. |
| `source.node.name`                              | string                       | Node the source pod ran on.                                                                                                                                                                                                                |
| `source.node.architecture`                      | string                       | Node CPU architecture, as `GOARCH` spells it.                                                                                                                                                                                              |
| `source.node.kernelVersion`                     | string                       | Node kernel release.                                                                                                                                                                                                                       |
| `source.node.cpuFeatures`                       | []string                     | Instruction-set flags from the node's `/proc/cpuinfo` that every processor reported, such as `avx512f` on x86_64 or `sve2` on arm64, sorted.                                                                                               |
| `source.pod.image`                              | string                       | Container image reference the checkpoint ran.                                                                                                                                                                                                 |
| `source.pod.imageDigest`                        | string                       | Identifies which build of the image ran, which a mutable tag does not.                                                                                                                                                                     |
| `source.pod.memory`                             | string                       | Container memory limit; absent if it had none.                                                                                                                                                                                             |
| `source.pod.cpu`                                | string                       | Container CPU limit; absent if it had none.                                                                                                                                                                                                |
| `source.devices.nvidia.driverVersion`           | string                       | NVIDIA driver the checkpoint ran against.                                                                                                                                                                                                     |
| `source.devices.nvidia.instances[].productName` | string                       | GPU model as `nvidia-smi` reports it, one entry per GPU the checkpointed container could see.                                                                                                                                                  |
| `storage.artifacts`                             | []PublishedContainerArtifact | Optional confirmed publications, keyed by `containerName`. Requires `spec.storage`; container names must be checkpointed containers. Does not replace Ready/Failed conditions.                                                                    |
| `storage.artifacts[].containerName`             | string                       | Required container name; entries must be unique.                                                                                                                                                                                           |
| `storage.artifacts[].artifactHandle`            | string                       | Required opaque publication locator within the bound store, up to 4096 characters. Callers must not interpret it as a filesystem path.                                                                                                     |

### SnapshotJob

Runs a checkpoint-ready workload pod and checkpoints it into a `PodSnapshot` in
one declarative object.

`spec`:

| Field                                  | Type            | Required | Default    | Description                                                                                                                                                                             |
|----------------------------------------|-----------------|----------|------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `podTemplate`                          | PodTemplateSpec | yes      | —          | The workload to run and checkpoint. The controller injects the snapshot contract (control volume, readiness probe, seccomp); image, command, GPU resources, and sidecars are the caller's. |
| `podSnapshotTemplate.targetContainers` | []string        | no       | `["main"]` | Container(s) to checkpoint. Exactly one in `v1alpha1`; each must name a container present in `podTemplate`.                                                                             |
| `podSnapshotTemplate.containers`       | []ContainerSnapshotConfig | no       | —          | [Container settings](#container-settings), copied to the generated `PodSnapshot`. Each `name` must be in `targetContainers`.                                                            |
| `activeDeadlineSeconds`                | int64           | no       | `3600`     | Total time allowed for scheduling, quiesce, and dump; applied to the batch/v1 Job.                                                                                                      |
| `nonTargetArtifacts.containers`        | []string        | no       | —          | Non-target containers that write artifacts to the snapshot volume. Must be nonempty if `nonTargetArtifacts` is set.                                                               |
| `nonTargetArtifacts.onFailure`         | string          | no       | `Retain`   | `Retain` keeps a failed attempt's artifacts. `Cleanup` lets the operator delete them once all writers have stopped.                                                                     |

`podSnapshotTemplate` itself is required; its fields are optional. The object's
`metadata.name` must be at most 63 characters (it is used as a label value).

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
| `Completed` | The snapshot is durable and the source Job has finished.                      |
| `Failed`    | A terminal failure occurred.                                                    |

### Container settings

An entry in `PodSnapshot.spec.containers` or
`SnapshotJob.spec.podSnapshotTemplate.containers`, of type
`ContainerSnapshotConfig`, sets lifecycle hooks for one checkpointed
container. Every field but `name` is optional. What the workload
must do for each hook is in the
[Workload Contract](workload-contract.md#lifecycle-hooks).

| Field                                          | Type          | Default | Description                                                                                  |
|------------------------------------------------|---------------|---------|----------------------------------------------------------------------------------------------|
| `name`                                         | string        | —       | Required. A container in `source.podRef.containers` (`targetContainers` in a `SnapshotJob`). |
| `checkpointReadinessProbe.file.path`           | string        | —       | Checkpoint once a regular file exists at this path in the container.                         |
| `checkpointReadinessProbe.httpGet.path`        | string        | `/`     | Checkpoint once a GET to this path on the Pod IP returns a 2xx status.                       |
| `checkpointReadinessProbe.httpGet.port`        | int or string | —       | Required with `httpGet`. A port number, or the name of one of the container's ports.         |
| `checkpointReadinessProbe.initialDelaySeconds` | int32         | `0`     | Seconds after the container starts before the first check.                                   |
| `checkpointReadinessProbe.periodSeconds`       | int32         | `1`     | Seconds between checks.                                                                      |
| `checkpointReadinessProbe.timeoutSeconds`      | int32         | `1`     | Seconds each check may take.                                                                 |
| `checkpointReadinessProbe.successThreshold`    | int32         | `1`     | Checks in a row that must succeed.                                                           |
| `lifecycle.postRestore.file.path`              | string        | —       | After restore, create an empty file at this path in the container.                           |
| `lifecycle.postRestore.httpPost.path`          | string        | `/`     | After restore, send a POST with an empty body to this path on the Pod IP.                    |
| `lifecycle.postRestore.httpPost.port`          | int or string | —       | Required with `httpPost`. A port number, or the name of one of the container's ports.        |
| `lifecycle.postRestore.periodSeconds`          | int32         | `1`     | Seconds between attempts.                                                                    |
| `lifecycle.postRestore.timeoutSeconds`         | int32         | `1`     | Seconds each attempt may take.                                                               |

A probe sets exactly one of `file` or `httpGet`, and `postRestore` exactly one
of `file` or `httpPost`. A file path is absolute and clean (no empty, `.` or
`..` parts) and at most 4096 characters, and the two hooks' file paths must
differ.

Without a `checkpointReadinessProbe`, the node agent checkpoints once the
container is Ready, as before. With one, the probe decides instead, and a
`SnapshotJob` keeps the container's own readiness probe rather than replacing
it with the `ready-for-snapshot` one. A probe the
agent can never run fails the `PodSnapshot` with reason
`InvalidCheckpointReadinessProbe`; while it waits, the source Pod gets a
`WaitingForCheckpointReadiness` event.

A restore succeeds only after its `postRestore` action succeeds, within
`config.restore.timeoutSeconds`; an agent restart can extend that by one
timeout, as
[Lifecycle hooks](../design/lifecycle-hooks/README.md#recovery-after-an-agent-restart)
explains. Its failures name the steps
`checking the post-restore action` and `running the post-restore action`; see
[Troubleshoot Restores](../troubleshooting/restores.md#checking-the-post-restore-action).

## Restore

The caller sets these annotations on the new pod to trigger a restore:

| Annotation                              | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
|-----------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `nvidia.com/restore-from`               | Names the `PodSnapshot`, in the pod's namespace, to restore into the pod.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `nvidia.com/restore-container-map`      | Optional. Comma-separated `source=destination` pairs mapping the single checkpointed container to one or more restore containers. When absent, the checkpointed container name is the destination.                                                                                                                                                                                                                                                                                                                                                                       |
| `nvidia.com/snapshot-skip-compat-check` | Optional. Set to `"true"` to attempt the restore without the compatibility checks, which otherwise refuse a snapshot this node cannot run. The value must parse as a boolean, so any other spelling, including `"yes"`, leaves the checks in place. Treat it as a debugging escape hatch: a restore that should have been refused instead fails somewhere inside CRIU. Adding it to a pod whose restore was already refused reopens that restore in place, so this is the one case that needs no new pod. A restore that was attempted and failed still does. |
| `nvidia.com/restore-retry-limit`        | Optional. How many more restore attempts each destination container gets after its first attempt fails, as a non-negative integer. Overrides `config.restore.retryLimit`, which defaults to `0`. Any other value fails the restore before its first attempt.                                                                                                                                                                                                                                                                                                  |

Snapshot then reports restore progress with a pod status condition, written by
the node agent and not set by the caller:

| Pod condition         | Description                                                                                                                                                         |
|-----------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `nvidia.com/Restored` | Added to the pod by the node agent once a restore is under way; becomes `True` when the restore completes. Observe it alongside pod readiness to confirm a restore. |

The node agent also writes this annotation once a restore attempt fails:

| Annotation                        | Description                                                                                                                                                                                                                                                                                                                  |
|-----------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `nvidia.com/restore-retry-status` | JSON with one entry per destination container that had a failed attempt. Each entry holds `attempt`, the current or last attempt from 1; `retryLimit`; and `lastFailure`, the `step` and `cause` of the last failed attempt. `lastFailure.snapshotUnusable` is `true` when the snapshot is missing, corrupt, or unsupported. |

All attempts of a restore share one deadline, `config.restore.timeoutSeconds`,
from the start of the first. Only a failure that left the container untouched
is retried; see [Restore Retries](restore-pod-contract.md#restore-retries).

## Labels

Snapshot sets and consumes these labels itself; callers do not set them. They are
listed for selection and debugging.

| Label                                  | Applied to                      | Purpose                                                                                                        |
|----------------------------------------|---------------------------------|----------------------------------------------------------------------------------------------------------------|
| `nvidia.com/snapshot-capture-eligible` | Source pod                      | Added by the agent's pre-bind gate after the source pod passes validation; the node agent's source-pod informer selects on it. |
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

A non-target container can write output to the snapshot volume. Declare it in `spec.nonTargetArtifacts.containers` (regular containers only, excluding target containers). Do not combine this field with the legacy `nvidia.com/snapshot-non-target-artifact-containers` annotation.

Snapshot injects `SNAPSHOT_JOB_UID` and `SNAPSHOT_NON_TARGET_ARTIFACT_SUBDIR` (`non-target-artifacts/<SnapshotJob UID>`, relative to the PVC root). Supply the PVC mount, write beneath that directory, and finish before the container exits. It must be writable by the container and removable by the operator. Restore consumers find the attempt through the `PodSnapshot`'s `nvidia.com/snapshot-job-uid` label.

On failure, `Retain` (the default) keeps the output. `Cleanup` deletes it only when the `SnapshotJob` is `Failed` and all source Pod containers are confirmed stopped. If proof cannot arrive, such as after Pod removal, Snapshot retains the directory and emits a `NonTargetArtifactsRetained` warning once per attempt per operator process. Verify writer exit before manual removal. Successful output is caller-managed. This applies only to the shared PVC, not S3 or snapshots.
