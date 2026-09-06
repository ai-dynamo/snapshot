# API reference

Snapshot's API is a set of Kubernetes custom resources, plus the annotations,
labels, pod conditions, and Helm configuration that drive them. All custom
resources are in API group `nvidia.com`, version `v1alpha1`.

For how these fit together, see [How to use it](../../README.md#how-to-use-it)
and the [usage guides](../guides/README.md).

## Custom resources

| Kind | Scope | Short name | Created by |
|------|-------|-----------|-----------|
| `PodSnapshot` | Namespaced | `podsnap` | The caller (or a controller / `SnapshotJob`) |
| `PodSnapshotContent` | Cluster | `podsnapcontent` | The operator only |
| `SnapshotJob` | Namespaced | `snapjob` | The caller |

Each resource's `spec` is immutable after creation.

### PodSnapshot

The namespaced binding for a captured container checkpoint; consumed by restore.

`spec`:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `source.podRef.name` | string | yes | Name of the source pod, in the same namespace. |
| `source.podRef.uid` | string | no | UID of the source pod, so the agent dumps that exact pod and not a same-named recreation. |
| `source.podRef.containers` | []string | yes | Container(s) to checkpoint. Exactly one in `v1alpha1`; each must be a DNS label. |

`status`:

| Field | Type | Description |
|-------|------|-------------|
| `boundSnapshotContentName` | string | Name of the bound cluster-scoped `PodSnapshotContent`; unset until the agent binds it. |
| `conditions` | []Condition | `Ready` (capture and binding complete, artifact usable for restore) and `Failed` (capture or binding failed terminally). |

### PodSnapshotContent

The cluster-scoped artifact-of-record for a checkpoint. The operator creates it
when it binds a `PodSnapshot`; callers never create it.

`spec`:

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `snapshotRef.namespace` | string | yes | Namespace of the bound `PodSnapshot`. |
| `snapshotRef.name` | string | yes | Name of the bound `PodSnapshot`. |
| `snapshotRef.uid` | string | no | UID recorded at binding time, to detect a delete-and-recreate. |
| `source.podRef` | PodReference | yes | The pod to dump (`name` / `uid` / `containers`). |
| `source.nodeName` | string | yes | Node the source pod runs on; selects the node agent that performs the dump. |

`status`: `conditions` — `Ready` and `Failed`.

### SnapshotJob

Runs a checkpoint-ready workload pod and captures it into a `PodSnapshot` in one
declarative object.

`spec`:

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `podTemplate` | PodTemplateSpec | yes | — | The workload to run and capture. The controller injects the snapshot contract (control volume, readiness probe, seccomp); image, command, GPU resources, and sidecars are the caller's. |
| `podSnapshotTemplate.targetContainers` | []string | no | `["main"]` | Container(s) to checkpoint. Exactly one in `v1alpha1`; each must name a container present in `podTemplate`. |
| `activeDeadlineSeconds` | int64 | no | `3600` | Total time allowed for scheduling, quiesce, and dump; applied to the batch/v1 Job. |

`podSnapshotTemplate` itself is required; its only field, `targetContainers`,
defaults. The object's `metadata.name` must be at most 63 characters (it is used
as a label value).

`status`:

| Field | Type | Description |
|-------|------|-------------|
| `podSnapshotName` | string | Name of the produced `PodSnapshot` (empty means never created). |
| `podSnapshotUID` | string | UID of the produced `PodSnapshot`. |
| `sourceJobUID` | string | UID of the source batch/v1 Job. |
| `startedAt` | time | When the source pod was first observed Ready. |
| `completedAt` | time | When a terminal condition was set. |
| `conditions` | []Condition | See below. |

`SnapshotJob` conditions (all four are always present; read `reason` and `message` for detail):

| Type | True when |
|------|-----------|
| `Running` | The source pod is running and ready. |
| `Captured` | The CRIU dump of the target container is complete (the `PodSnapshot` is Ready). |
| `Completed` | The checkpoint is durable and the source Job has finished. |
| `Failed` | A terminal failure occurred. |

## Restore

The caller sets these annotations on the new pod to trigger a restore:

| Annotation | Description |
|------------|-------------|
| `nvidia.com/restore-from` | Names the `PodSnapshot`, in the pod's namespace, to restore into the pod. |
| `nvidia.com/restore-container-map` | Optional. Comma-separated `source=destination` pairs mapping the single captured container to one or more restore containers. When absent, the captured container name is the destination. |

Snapshot then reports restore progress with a pod status condition — written by
the node agent, not set by the caller:

| Pod condition | Description |
|---------------|-------------|
| `nvidia.com/Restored` | Added to the pod by the node agent once a restore is under way; becomes `True` when the restore completes. Observe it alongside pod readiness to confirm a restore. |

## Labels

Snapshot sets and consumes these labels itself; callers do not set them. They are
listed for selection and debugging.

| Label | Applied to | Purpose |
|-------|-----------|---------|
| `nvidia.com/snapshot-capture-eligible` | Source pod | Added by the agent's pre-bind gate after the source pod passes validation; the capture informer selects on it. |
| `nvidia.com/snapshot-node` | `PodSnapshotContent` | Mirrors `spec.source.nodeName` so each node agent can select its own work. |
| `nvidia.com/snapshot-job` | `PodSnapshot` | Maps a produced `PodSnapshot` back to the `SnapshotJob` that created it. |
| `nvidia.com/snapshot-job-uid` | `SnapshotJob`-created resources | Binds them to one `SnapshotJob` incarnation, since names can be reused. |

## The snapshot-control volume

Checkpoint and restore are coordinated through a per-pod `emptyDir` that the
workload and the node agent share.

| Item | Value | Description |
|------|-------|-------------|
| Volume name | `snapshot-control` | Per-pod `emptyDir`. With multiple target containers, each mounts it with `subPath=<containerName>`. |
| Mount path | `/snapshot-control` | Where the workload sees the control directory. |
| Environment | `SNAPSHOT_CONTROL_DIR` | Exposes the mount path to the workload (legacy name: `DYN_SNAPSHOT_CONTROL_DIR`). |

Sentinel files inside the volume:

| File | Written by | Meaning |
|------|-----------|---------|
| `ready-for-snapshot` | Workload | The model is loaded and it is safe to checkpoint. The source readiness probe gates on this. |
| `restore-complete` | Node agent | The restore finished and the workload may resume. |
| `cuda-checkpoint-job` | Node agent | The persisted CUDA checkpoint job file. |

A checkpoint always terminates the source process, so there is no
`snapshot-complete` sentinel. Checkpointing also requires a seccomp profile that
blocks io_uring (which CRIU cannot checkpoint); the chart installs it at
`profiles/block-iouring.json`.

## Configuration

Snapshot is configured through the Helm chart. The values below show their
defaults; see [Storage](../operations/storage.md) for the storage model.

### Images and rollout

| Value | Default | Description |
|-------|---------|-------------|
| `image.operator.repository` | `ghcr.io/ai-dynamo/snapshot/operator` | Operator image. |
| `image.agent.repository` | `ghcr.io/ai-dynamo/snapshot/agent` | Agent image. |
| `image.*.tag` | chart `appVersion` | Image tag; defaults to the chart's `appVersion` when empty. |
| `crdUpgrade.enabled` | `true` | Re-apply the CRDs on every rollout via an init container. |
| `runtime.type` | `containerd` | Container runtime: `containerd` or `crio`. |
| `runtime.socketPath` | `""` | Runtime socket path; empty uses the conventional path for the type. |
| `openshift.enabled` | `false` | Enable OpenShift RBAC/SCC pieces. Keep `false` on vanilla Kubernetes. |

### Storage

| Value | Default | Description |
|-------|---------|-------------|
| `storage.type` | `pvc` | Only `pvc` is implemented today. |
| `storage.pvc.create` | `true` | Create the PVC; set `false` to use an existing one. |
| `storage.pvc.name` | `snapshot-pvc` | Shared PVC name. |
| `storage.pvc.size` | `1Ti` | Requested size. |
| `storage.pvc.storageClass` | `""` | Storage class; empty uses the cluster default. Must support `ReadWriteMany`. |
| `storage.pvc.basePath` | `/checkpoints` | Fixed agent mount path; cannot be changed. |

### Agent DaemonSet and access

| Value | Default | Description |
|-------|---------|-------------|
| `daemonset.snapshotLogLevel` | `info` | Agent log level (`trace`/`debug`/`info`/`warn`/`error`). |
| `daemonset.resources` | 4 CPU / 4Gi limit | Agent resource requests and limits. |
| `daemonset.nodeSelector` | `nvidia.com/gpu.present: "true"` | Targets GPU nodes. |
| `daemonset.tolerations` | GPU + `dedicated` | Node tolerations. |
| `daemonset.imagePullSecrets` | `ngc-secret` | Pull secrets for the agent image. |
| `seccomp.deploy` | `true` | Install the block-iouring seccomp profile (required for CRIU; set `false` on RHCOS 9.6+). |
| `rbac.create` | `true` | Create agent and operator RBAC. |
| `serviceAccount.create` | `true` | Create the agent service account. |

### Agent config (`config.*`)

`config.*` renders into the agent ConfigMap.

- `config.overlay.exclusions` — rootfs-diff tar exclusions. Default: `/proc`, `/sys`, `/dev`, `*/.cache/huggingface`, `*/__pycache__`, `*.pyc`.
- `config.restore.restoreTimeoutSeconds` — maximum seconds for a restore before the agent marks it failed. Default `7200`.

`config.criu.*` — CRIU options:

| Value | Default | Description |
|-------|---------|-------------|
| `binaryPath` | `/usr/local/sbin/criu` | Path to the criu binary. |
| `ghostLimit` | `536870912` | Max size in bytes of a deleted-but-open file saved inline as a ghost file. |
| `logLevel` | `4` | CRIU verbosity (0–4). |
| `workDir` | `/var/criu-work` | CRIU temporary-file directory. |
| `shellJob` | `true` | Treat containers as session leaders. |
| `tcpClose` | `false` | Close non-listening TCP sockets on restore. |
| `tcpEstablished` | `true` | Preserve established TCP sockets. `tcpClose` and `tcpEstablished` cannot both be `true`. |
| `fileLocks` | `true` | Preserve file locks. |
| `orphanPtsMaster` | `true` | Support containers with TTYs. |
| `extUnixSk` | `true` | External Unix sockets. |
| `linkRemap` | `true` | Support deleted-but-open files (e.g. `/dev/shm` semaphores). |
| `extMasters` | `true` | External bind-mount masters. |
| `manageCgroupsMode` | `soft` | CRIU cgroup mode: `ignore` / `soft` / `full` / `strict`. |
| `imageIoMode` | `direct` | CRIU image I/O: `writeback` or `direct`. |
| `rstSibling` | `true` | Restore as a sibling process (required for go-criu swrk mode). |
| `mntnsCompatMode` | `false` | Mount-namespace compatibility mode, applied during restore. |
| `evasiveDevices` | `true` | Use any device path when the original is inaccessible. |
| `forceIrmap` | `true` | Force resolving inotify/fsnotify watch names. |
| `autoDedup` | `false` | Auto-deduplicate memory pages. |
| `lazyPages` | `false` | Lazy page migration (experimental). |
| `libDir` | `/usr/local/lib/snapshot/criu-plugins` | CRIU plugin directory used by the chart. |
| `allowUprobes` | `true` | Kernel/userspace probe compatibility. |
| `skipInFlight` | `true` | Skip in-flight TCP connections. |
