# API Reference

Use this page to look up the Snapshot API. It lists the five custom resources,
`PodSnapshot`, `PodSnapshotContent`, `SnapshotJob`, `PodGroupSnapshot`, and
`PodGroupRestore`, and the annotations, labels, pod conditions, and control
volume that work with them. All custom resources are in API group `nvidia.com`,
version `v1alpha1`.

To see how these fit together, read [Core Concepts](../overview/core-concepts.md).
To use them in a checkpoint and restore, follow
[Using Snapshot](../using-snapshot/README.md). For chart settings, see
[Helm Chart Values](helm-values.md).

## Custom Resources

| Kind                 | Scope      | Short name        | Created by                                   |
|----------------------|------------|-------------------|----------------------------------------------|
| `PodSnapshot`        | Namespaced | `podsnap`         | The caller (or a controller / `SnapshotJob`) |
| `PodSnapshotContent` | Cluster    | `podsnapcontent`  | The operator only                            |
| `SnapshotJob`        | Namespaced | `snapjob`         | The caller                                   |
| `PodGroupSnapshot`   | Namespaced | `podgroupsnap`    | The caller                                   |
| `PodGroupRestore`    | Namespaced | `podgrouprestore` | The caller                                   |

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
| `podGroup` | PodGroupSnapshotMembership | no | Coordinated checkpoint membership. Omitted means standalone. See [Pod Group Membership](#pod-group-membership). |

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
| `podGroup` | PodGroupSnapshotMembership | no | Coordinated checkpoint membership. Omitted means standalone. See [Pod Group Membership](#pod-group-membership). |

`status`:

| Field                                           | Type                         | Description                                                                                                                                                                                                                                |
|-------------------------------------------------|------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `conditions`                                    | []Condition                  | `Ready` (snapshot taken and usable for restore) and `Failed` (checkpoint failed terminally). For a member of a pod group, also `Started`: set once the group's checkpoint commits, before the node agent changes the source, and never removed. |
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

An entry in `PodSnapshot.spec.containers`,
`SnapshotJob.spec.podSnapshotTemplate.containers`, or
`PodGroupSnapshot.spec.members[].podSnapshotTemplate.containers`, of type
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

### Pod Group Membership

`PodGroupSnapshotMembership` identifies one member of a coordinated checkpoint:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Name of the `PodGroupSnapshot`, a valid object name of at most 253 characters. |
| `uid` | string | yes | UID of the group, 1 to 128 characters. |
| `memberID` | string | yes | Member ID in the group, a DNS label of at most 63 characters. |

The group controller sets the membership on each member's `PodSnapshot`.
The snapshot controller copies it unchanged into `PodSnapshotContent`.
A content with membership is checkpointed with its group, never alone.
The group is in the snapshot's namespace, or `spec.snapshotRef.namespace` on
cluster-scoped content. Membership is immutable with the rest of the spec.

### PodGroupSnapshot

> [!NOTE]
> Pod groups are alpha. For how a group checkpoints and restores, see
> [SNEP-324: Coordinated multi-Pod snapshot and restore](https://github.com/ai-dynamo/snapshot/pull/325).

Checkpoints a fixed group of cooperating Pods together, so that a
`PodGroupRestore` can restore them together. Its `metadata.name` must be at
most 63 characters.

`spec`:

| Field                                      | Type                        | Required | Description                                                                                                                                                                                                            |
|--------------------------------------------|-----------------------------|----------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `deadlineSeconds`                          | int64                       | yes      | Bounds the whole checkpoint, counted from the object's creation: 1 to 86400 (a day). Members still being checkpointed when it expires fail with `DeadlineExceeded`; members already checkpointed keep their snapshots. |
| `members`                                  | []PodGroupSnapshotMember    | yes      | The Pods to checkpoint together, 1 to 256. A member's position in the list is its rank, which every restore of the group reuses.                                                                                       |
| `members[].id`                             | string                      | yes      | Names the member from checkpoint to restore: the `PodGroupRestore` target with the same ID restores it. A DNS label, unique in the group.                                                                              |
| `members[].source.podRef.name`             | string                      | yes      | Name of the source Pod, in the same namespace.                                                                                                                                                                         |
| `members[].source.podRef.uid`              | string                      | yes      | UID of the source Pod, so a recreated Pod with the same name is never checkpointed in its place.                                                                                                                       |
| `members[].source.podRef.containers`       | []string                    | yes      | Container to checkpoint. Exactly one in `v1alpha1`; it must be a DNS label.                                                                                                                                            |
| `members[].podSnapshotTemplate.metadata`   | PodSnapshotTemplateMetadata | no       | `labels` and `annotations` copied to the member's `PodSnapshot`, at most 4096 of each. Keys must be qualified names, and label values at most 63 characters.                                                           |
| `members[].podSnapshotTemplate.containers` | []ContainerSnapshotConfig   | no       | [Container settings](#container-settings) for the member's checkpointed container. At most one entry, whose `name` must be in `source.podRef.containers`.                                                              |

A member without a `podSnapshotTemplate` gets the default settings of a
`PodSnapshot`. The template's labels must not set
`nvidia.com/snapshot-job` or `nvidia.com/snapshot-job-uid`, which the
controllers own.

With 256 members whose templates hold very long values, the object and its
status can exceed the API server's object size limit of about 1.5 MiB.

`status`:

| Field                   | Type                           | Description                                                                                                                                                                                                                            |
|-------------------------|--------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `members`               | []PodGroupSnapshotMemberStatus | Each member's progress, keyed by `id`, at most 256. An entry names a member of `spec.members` and, once written, is never removed.                                                                                                     |
| `members[].id`          | string                         | The member's ID in `spec.members`.                                                                                                                                                                                                     |
| `members[].rank`        | int32                          | The member's position in `spec.members`, from 0 to 255.                                                                                                                                                                                |
| `members[].nodeName`    | string                         | Node the source Pod runs on.                                                                                                                                                                                                           |
| `members[].podSnapshot` | ObjectIdentity                 | `name` and `uid` of the `PodSnapshot` that the `PodGroupSnapshot` owns for this member.                                                                                                                                                |
| `members[].content`     | ObjectIdentity                 | `name` and `uid` of the member's `PodSnapshotContent`, which records its snapshot.                                                                                                                                                     |
| `members[].startedAt`   | time                           | When Snapshot began changing the source Pod: the time of the `Started` condition that the node agent sets on the member's `PodSnapshotContent` before its first irreversible step. Absent means Snapshot never changed the source Pod. |
| `members[].phase`       | string                         | The member's progress, from the table below. Defaults to `Pending`.                                                                                                                                                                    |
| `members[].reason`      | string                         | A CamelCase word that explains the phase, such as `DeadlineExceeded` for a `Failed` member, at most 1024 characters. See [Pod Group Conditions](#pod-group-conditions).                                                                |
| `members[].message`     | string                         | A human-readable explanation of the phase, at most 1024 characters.                                                                                                                                                                    |
| `conditions`            | []Condition                    | `Admitted`, `Ready`, and `Failed`. See [Pod Group Conditions](#pod-group-conditions).                                                                                                                                                  |

A member's `nodeName`, `podSnapshot`, `content`, and `startedAt` are
write-once. Its phase is one of these:

| Phase           | Meaning                                                        |
|-----------------|----------------------------------------------------------------|
| `Pending`       | The member has not yet passed admission.                       |
| `Admitted`      | The member passed admission and has its `PodSnapshot`.         |
| `Checkpointing` | The member is being checkpointed.                              |
| `Checkpointed`  | The member's snapshot is complete. Final.                      |
| `Failed`        | Checkpointing the member failed; its `reason` says why. Final. |

### PodGroupRestore

Restores every member of a `PodGroupSnapshot` into its own restore Pod, as one
attempt. A `PodGroupSnapshot` can be restored many times; each
`PodGroupRestore` is a new attempt with new restore Pods. Each restore Pod must
be a group target, as
[Group Restore Targets](restore-pod-contract.md#group-restore-targets)
describes.

`spec`:

| Field                         | Type                    | Required | Description                                                                                                                                                                                                                                            |
|-------------------------------|-------------------------|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `snapshotRef.name`            | string                  | yes      | Name of the `PodGroupSnapshot` to restore, in this namespace, at most 253 characters.                                                                                                                                                                  |
| `snapshotRef.uid`             | string                  | yes      | UID of that `PodGroupSnapshot`, at most 128 characters, so a recreated one with the same name is never restored in its place.                                                                                                                          |
| `deadlineSeconds`             | int64                   | yes      | Bounds the whole restore, counted from the object's creation: 1 to 86400 (a day). Members still restoring when it expires fail with `DeadlineExceeded`; members already `Released` keep running.                                                       |
| `targets`                     | []PodGroupRestoreTarget | yes      | The restore Pods, one for each member of the `PodGroupSnapshot`: 1 to 256, keyed by `id`. Their order carries no meaning.                                                                                                                              |
| `targets[].id`                | string                  | yes      | ID of the `PodGroupSnapshot` member that this target restores. A DNS label. The target takes that member's rank.                                                                                                                                       |
| `targets[].podRef.name`       | string                  | yes      | Name of the restore Pod, in this namespace, at most 253 characters. The Pod may not exist yet when the `PodGroupRestore` is created.                                                                                                                   |
| `targets[].podRef.uid`        | string                  | no       | UID of the restore Pod, at most 128 characters. Without it, the target is the Pod with this name at admission, and its UID is recorded in `status`.                                                                                                    |
| `targets[].containerMappings` | []ContainerMapping      | no       | Exactly one pair: `source`, the member's checkpointed container, and `destination`, the restore Pod's container that receives its processes. Each is a DNS label. Without it, the checkpointed container restores into the container of the same name. |

`status`:

| Field                   | Type                             | Description                                                                                                                                                                                                                         |
|-------------------------|----------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `members`               | []PodGroupRestoreMemberStatus    | Each target's progress, keyed by `id`, at most 256. An entry names a target of `spec.targets` and, once written, is never removed.                                                                                                  |
| `members[].id`          | string                           | The target's ID in `spec.targets`.                                                                                                                                                                                                  |
| `members[].pod`         | ObjectIdentity                   | `name` and `uid` of the restore Pod, pinned when the operator admits the target.                                                                                                                                                    |
| `members[].nodeName`    | string                           | Node the restore Pod runs on, pinned at admission.                                                                                                                                                                                  |
| `members[].containers`  | []PodGroupRestoreContainerStatus | The destination container's `name`, and its runtime `containerID` when the operator pinned it at admission. One entry.                                                                                                              |
| `members[].targetPodIP` | string                           | The restore Pod's IPv4 address, pinned at admission.                                                                                                                                                                                |
| `members[].startedAt`   | time                             | When Snapshot began restoring into the restore Pod: the time of the Pod's `nvidia.com/RestoreStarted` condition, which the node agent sets before its first irreversible step. Absent means Snapshot never changed the restore Pod. |
| `members[].phase`       | string                           | The member's progress, from the table below. Defaults to `Pending`.                                                                                                                                                                 |
| `members[].reason`      | string                           | A CamelCase word that explains the phase, such as `PodIPChanged` for a `Failed` member, at most 1024 characters. See [Pod Group Conditions](#pod-group-conditions).                                                                 |
| `members[].message`     | string                           | A human-readable explanation of the phase, at most 1024 characters.                                                                                                                                                                 |
| `conditions`            | []Condition                      | `Admitted`, `Released`, `Ready`, and `Failed`. See [Pod Group Conditions](#pod-group-conditions).                                                                                                                                   |

A member's `pod`, `nodeName`, `containers`, `targetPodIP`, and `startedAt` are
write-once. Its phase is one of these:

| Phase       | Meaning                                                                                                                                                                      |
|-------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Pending`   | The member has not yet passed admission.                                                                                                                                     |
| `Admitted`  | The member passed admission, and its target is claimed and pinned.                                                                                                           |
| `Restoring` | The member is being restored, or waits for its `postRestore` notification to succeed.                                                                                        |
| `Released`  | The member reached its release point, its [`restore-complete`](restore-pod-contract.md#group-restore-targets), and any `postRestore` notification has also succeeded. Final. |
| `Failed`    | The member's restore failed; its `reason` says why. Final.                                                                                                                   |

### Pod Group Conditions

Read each condition's `reason` and `message` for detail.

| Type       | True when                                                                                                                                                                                      |
|------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Admitted` | The attempt is committed, with reason `Committed`.                                                                                                                                             |
| `Released` | It turns True on the first `Released` member of a `PodGroupRestore`, which proves that the agents' release barrier completed. `Released` members keep running whatever happens to their peers. |
| `Ready`    | Every member of a `PodGroupSnapshot` is `Checkpointed`, or every member of a `PodGroupRestore` is `Released`.                                                                                  |
| `Failed`   | The attempt failed terminally.                                                                                                                                                                 |

Before the commit, `Admitted` is `False`: with reason `Admitting` while the
members' work is created and admitted, and with the reason of the wait or the
failure otherwise.

These reasons appear on the conditions and on the members' statuses:

| Reason                 | Meaning                                                                                                                                                                                           |
|------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `Refused`              | The attempt ended before Snapshot changed any member, for example because admission refused a member.                                                                                             |
| `DeadlineExceeded`     | `spec.deadlineSeconds` expired.                                                                                                                                                                   |
| `MemberFailed`         | A member failed after admission, which failed the attempt.                                                                                                                                        |
| `CoordinatorLost`      | The attempt lost its coordination session, for example because the coordinator restarted.                                                                                                         |
| `Cancelled`            | The attempt was cancelled because its group was deleted. A group reports it only when no member started; a member can report it after starting, when the deletion raced the commit.               |
| `ArtifactUnavailable`  | A member's snapshot cannot be restored.                                                                                                                                                           |
| `TargetInUse`          | The target Pod is already claimed by another `PodGroupRestore`.                                                                                                                                   |
| `TargetPodPending`     | Admission is waiting for a restore target's Pod: it does not exist yet, or, once claimed, has no IP address or destination container ID yet. The message names each target and what it waits for. |
| `TargetPodUnscheduled` | Admission is waiting for a claimed restore target's Pod to be scheduled to a node, typically for lack of capacity. The message names each target.                                                 |
| `TargetPodDeleted`     | A restore member's pinned Pod is gone or was replaced. The member may have no `startedAt`, since the Pod's `nvidia.com/RestoreStarted` condition went with it.                                    |
| `PodIPChanged`         | A restore Pod's IP or admitted container changed after the operator pinned it.                                                                                                                    |

## Restore

The caller sets these annotations on the new pod to trigger a restore:

| Annotation                              | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
|-----------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `nvidia.com/restore-from`               | Names the `PodSnapshot`, in the pod's namespace, to restore into the pod.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `nvidia.com/restore-container-map`      | Optional. Comma-separated `source=destination` pairs mapping the single checkpointed container to one or more restore containers. When absent, the checkpointed container name is the destination.                                                                                                                                                                                                                                                                                                                                                                       |
| `nvidia.com/snapshot-skip-compat-check` | Optional. Set to `"true"` to attempt the restore without the compatibility checks, which otherwise refuse a snapshot this node cannot run. The value must parse as a boolean, so any other spelling, including `"yes"`, leaves the checks in place. Treat it as a debugging escape hatch: a restore that should have been refused instead fails somewhere inside CRIU. Adding it to a pod whose restore was already refused reopens that restore in place, so this is the one case that needs no new pod. A restore that was attempted and failed still does. |

Snapshot then reports restore progress with a pod status condition, written by
the node agent and not set by the caller:

| Pod condition                   | Description                                                                                                                                                         |
|---------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `nvidia.com/Restored`           | Added to the pod by the node agent once a restore is under way; becomes `True` when the restore completes. Observe it alongside pod readiness to confirm a restore. |
| `nvidia.com/RestoreStarted`     | Group restore targets only. `True` once the group's restore commits, before the node agent changes the pod. Never removed.                                         |
| `nvidia.com/GroupRestoreFailed` | Group restore targets only. `True` when the member failed for a reason of the group, which is its reason. Set with `nvidia.com/Restored` `False`, `RestoreFailed`. |

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
