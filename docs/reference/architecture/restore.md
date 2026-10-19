<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Restore flow

<!-- toc -->
- [The destination pod](#the-destination-pod)
- [Container creation](#container-creation)
  - [Nodes without the restore proxy](#nodes-without-the-restore-proxy)
- [Preflight](#preflight)
- [Inspect and compatibility](#inspect-and-compatibility)
- [Mounts and the transaction](#mounts-and-the-transaction)
- [Inside the container](#inside-the-container)
  - [CRIU restore options and rewrites](#criu-restore-options-and-rewrites)
- [GPU restore](#gpu-restore)
- [Completion](#completion)
- [Outcomes and reporting](#outcomes-and-reporting)
- [Multi-GPU hooks](#multi-gpu-hooks)
- [Invariants](#invariants)
<!-- /toc -->

This page is for contributors.
It follows one restore from a pod carrying `nvidia.com/restore-from` to a running, restored process and a terminal `nvidia.com/Restored` condition.
The [node agent](node-agent.md) page describes the processes involved.
To restore a workload, follow [Restore a replica](../../guides/restore.md) and the [restore pod contract](../restore-pod-contract.md) instead.

The operator plays no part in a restore.
Two parts of the agent do, independently:

- The **NRI plugin** acts when the container runtime creates the destination container, before it starts. It makes the restore proxy PID 1.
- The **node controller** acts once the pod is running on the node. It validates, restores, and reports.

The [benchmarks](../../development/benchmarks.md) divide a restore into four stages.
They map onto this page as follows:

| Benchmark stage | Sections below |
| --- | --- |
| Agent setup | [Preflight](#preflight), [Inspect and compatibility](#inspect-and-compatibility), [Mounts and the transaction](#mounts-and-the-transaction) |
| CRIU restore | [Inside the container](#inside-the-container), up to and including the CRIU restore |
| CUDA restore | [GPU restore](#gpu-restore) |
| Wake and remap | The address rewrite in [CRIU restore options and rewrites](#criu-restore-options-and-rewrites), the device map in [GPU restore](#gpu-restore), and [Completion](#completion) |

## The destination pod

A restore pod is an ordinary pod with the contract's additions: the annotation, the `snapshot-control` `emptyDir` mounted with `subPath` equal to the container name, `SNAPSHOT_CONTROL_DIR`, the `block-iouring` seccomp profile, and optionally a startup probe on `restore-complete`.
`podcontract.Build` in `api/podcontract` produces that shape, and `podcontract.Validate` checks the minimum of it.
The destination container keeps the source's command in its spec.
That command never runs.

The `nvidia.com/restore-container-map` annotation can fan one captured container out to several destinations.
Each destination is restored separately, and the pod's condition aggregates the results.

## Container creation

When the kubelet asks the runtime to create a container in a pod with `nvidia.com/restore-from`, the runtime calls the agent's NRI plugin.
The plugin:

1. Resolves which containers are destinations. Without a container map it reads the `PodSnapshot` for the captured container's name, so the `PodSnapshot` must exist, but it need not be `Ready` yet.
2. Copies `snapshot-restore-proxy` from the agent's bundle into the container's `snapshot-control` subdirectory under the kubelet's pod directory on the host.
3. Replaces the container's OCI arguments with `/snapshot-control/snapshot-restore-proxy`.

It changes nothing else.
Seccomp, mounts, and environment come from the pod spec.
An error fails the container's creation and the kubelet retries, since running the image's own command would cold-start the workload in a container the agent is about to restore into.

The container then starts with the proxy as PID 1.
The proxy exits with code 112 at once if the control directory already holds `restore-complete` or `restore-failed`, which is how a pod gets exactly one restore across container restarts.
Otherwise it waits.

### Nodes without the restore proxy

NRI 0.10, shipped with containerd 2.2, is the first version that lets a plugin change a container's arguments.
On an older runtime, or one with NRI disabled, the agent fails to register at startup, logs that it is restoring without the proxy, and sets a flag that skips the PID 1 check below.
The container's own command then stays PID 1 and the restored process runs beside it.
The [restore pod contract](../restore-pod-contract.md#nodes-without-the-restore-proxy) lists what that costs: an inert command is required, the exit code is lost, and signals do not reach the workload.

## Preflight

The controller's restore informer delivers every pod on the node that carries the annotation or the agent's finalizer.
`preflightRestore` in `agent/internal/controller/controller.go` checks, with uncached reads:

| Check | While it fails |
| --- | --- |
| The `PodSnapshot` exists, is not deleting, is `Ready`, and is bound to a content | Condition `Restored=False` with reason `SnapshotPending`, requeue |
| The content exists, its back reference UID matches, and it is `Ready` | `ContentPending`, requeue |
| The pod passes `podcontract.Validate` and the content names exactly one container | `RestoreFailed` |
| The artifact directory exists | `ArtifactPending`, requeue |
| The preflight compatibility gate passes | `RestoreIncompatible`, terminal |
| The pod has an IP, when `tcpEstablished` was recorded | `PodIPPending`, requeue |

The preflight compatibility gate compares the manifest with the pod spec alone: CPU architecture, kernel version and minimum, and the CPU and memory limits.
[Compatibility checks](storage.md#compatibility-checks) lists every rule.
A pod already refused for incompatibility is reconsidered only if it gains the `nvidia.com/snapshot-skip-compat-check` annotation.

A pod that already carries a terminal `Restored` condition, for example after an agent restart, is left alone.
Its finalizer is removed and an informational event records that the restore was already complete.

When preflight passes, the agent adds the `snapshot/restore-protection` finalizer, writes `Restored=False` with reason `RestoreInProgress`, and starts one worker per destination container.
Each worker resolves its container ID from the pod status, polling the runtime for up to thirty seconds if the status has not caught up, emits a `RestoreRequested` event, and calls `executor.Restore` under `restoreTimeoutSeconds`.

## Inspect and compatibility

`Restore` in `agent/internal/executor/restore.go` begins by reading the manifest from the artifact and checking that its content UID and container name match the request.
If the manifest records CuInterpose libraries, the libraries in the agent's bundle must hash to the same values.
If it records a CustomStorage capture, PageBroker on this node must support CustomStorage.

`inspectRestore` then looks at the destination container:

- Its host PID and OCI spec, through the runtime.
- That PID 1 is the restore proxy, unless the node runs without it.
- Its image ID.
- Its cgroup v2 path, under which CRIU will place the restored tree.
- Its AppArmor or SELinux label from the OCI spec, which CRIU applies to the restored processes.
- Its GPUs, discovered the same way as at capture and in the same ordinal order.
- A **device map** from source GPU UUIDs to destination UUIDs. A UUID present on both sides maps to itself and the rest pair up by position. An all-identity map is omitted so that CUDA takes its default path.
- **Mount aliases** for GPUs whose `/dev/nvidiaN` path differs between capture and destination.

With that in hand it runs the inspect compatibility gate: image digest, externalized mounts, GPU model, GPU count, and driver version and minimum.
The GPU count must match even when the gates are skipped, because a per-GPU device state has nowhere to land otherwise.

A refusal at either gate is `IncompatibleError`.
No CRIU work was attempted, and retrying on this node cannot change the answer.
The controller reports `RestoreIncompatible` and **leaves the container running**, since killing it would restart it straight back into the same refusal.

## Mounts and the transaction

The destination image ships none of Snapshot's tooling, so the agent brings it along.
Through `ns-bind-mount`, the C helper with a fixed table of allowed mounts, it installs into the container's mount namespace:

| Source in the agent | Destination in the container | Flags |
| --- | --- | --- |
| `/snapshot-binaries` | `/tmp/snapshot-binaries` | read-only, `nosuid`, `nodev` |
| `/snapshot-binaries/snapshot-cuda` | `/tmp/snapshot-cuda` | read-only, `nosuid`, `nodev`, only for CuInterpose checkpoints |
| `/checkpoints/artifacts/<uid>/containers/<name>` | `/tmp/checkpoint` | read-only, `nosuid`, `nodev`, `noexec` |
| `/pagebroker/staging/restore/<txn>` | `/tmp/pagebroker` | read-only, `nosuid`, `nodev`, `noexec`, only in staged mode |

The helper clones the source with `open_tree`, applies the attributes with `mount_setattr`, enters the target namespace, and moves the clone into place.
Storage paths are validated in Go and again in C, and symlinks are rejected.

The agent then opens a PageBroker transaction for the restore.
In the default `direct` mode PageBroker pins the artifact directory and CRIU reads it through the `/tmp/checkpoint` mount.
In `staged` mode PageBroker first copies the CPU images, but not the `gpu/` payload, into its memory-backed staging area, and CRIU reads from `/tmp/pagebroker`.
For a CustomStorage checkpoint the prepare request also returns PageBroker's pidfd and an execution socket that `nsrestore` will connect to from inside the container.

## Inside the container

Everything from here to the GPU unlock happens inside the destination container's namespaces.
The agent runs its `nsrestore` binary there through `nsenter` into the mount, UTS, IPC, network, and PID namespaces, with the container's root.
The binary is passed as an inherited file descriptor so it can exec after the root changes, and the mount namespace is held open by a file descriptor so it survives even if the proxy exits.
`nsrestore` sets `PATH` and `LD_LIBRARY_PATH` to the bundle, prints its result as JSON on stdout, and forwards its logs to the agent's stderr.

`RestoreInNamespace` in `agent/internal/executor/nsrestore.go` then runs, in order:

1. **Replay the root filesystem diff.** `rootfs-diff.tar` is copied to local disk and extracted over `/` with the bundle's `tar`, using `--skip-old-files` so nothing in the image is overwritten, then the paths in `deleted-files.json` are removed. The destination's root is the runtime's own overlay of the same image, so after this step the files the processes expect are where they were. No new overlay is mounted.
2. **Place the CUDA job file**, for native multi-GPU checkpoints, at `/snapshot-control/cuda-checkpoint-job`.
3. **Install GPU mount aliases.** When the destination GPU has a different `/dev/nvidiaN` path, the destination device is bind mounted over the path CRIU will reopen.
4. **Unmount `/dev/shm`** so that CRIU recreates it with the checkpointed contents.
5. **Remount `/proc/sys` read-write** for the duration of the CRIU restore.
6. **Open the helper binaries by file descriptor.** `cuda-checkpoint-helper` and `cuinterpose-coordinator` are opened now, because the mount namespace CRIU creates for the restored tree does not contain the bundle.
7. **Remove stale sentinels** from `/snapshot-control` and remove any `/run/criu.kdat` the diff carried over, so CRIU probes this kernel's features itself.
8. **Mount securityfs** temporarily if the destination has an AppArmor label and securityfs is not visible, and unmount it again in CRIU's pre-restore hook.
9. **Run CRIU.** See the next section. CRIU reports the restored root PID, in the container's PID namespace.
10. **Map PIDs.** The CUDA PIDs recorded in the manifest are matched to the restored processes by walking `/proc`.
11. **Restore native GPU paths** in each restored mount namespace, undoing the aliases.
12. **Restore GPU state.** See [GPU restore](#gpu-restore).
13. **Run the CuInterpose coordinator** with `--restore`, when the manifest records one, after GPU state is back and before the workload resumes.

### CRIU restore options and rewrites

Restore uses the CRIU settings recorded in the manifest, so a checkpoint restores the way it was dumped, plus options that only make sense on the destination:

| Option | Value | Why |
| --- | --- | --- |
| `root` | `/` | `nsrestore` already runs with the container's root. |
| `ext_mnt` | every externalized mount, mapped to itself | The runtime recreated the OCI mounts in the destination. |
| Inherited network namespace | `/proc/1/ns/net` | The destination pod's namespace. |
| Inherited stdio | `/proc/1/fd/0`, `1`, `2` | The proxy's descriptors, so output reaches the container log. |
| `cgroup_root` | the destination's cgroup | Restored tasks land where the kubelet accounts them. |
| `lsm_profile` | the destination's label, or `none:` | Labels come from the destination, never from the checkpoint. |
| `network_lock` | skip | The namespace is a fresh one with no dump-time rules, and skipping avoids forking `iptables-restore`, which the bundle does not carry. |
| Plugin directory and binary | the bundle | Never the checkpoint's or the image's. |
| Configuration file | generated fresh | Never read from the checkpoint. |

Two rewrites happen before CRIU starts.
Both exist because a checkpoint can be restored into several pods that share nothing but the node, or into several containers that share one pod.

- **`files.img`** is re-encoded and overlaid read-only on the checkpoint's copy. Bound abstract Unix socket names get a suffix derived from the destination's mount namespace, so two restores in one network namespace do not collide. Established TCP connections whose both ends are in the image get a fresh client port. Connections to a peer outside the image are restored closed, because the remote end cannot follow a cloned connection.
- **The pod IP.** When the checkpoint preserved established connections and the destination has a different IP, the agent writes a one-line map file and the `snapshot_inet_remap.so` plugin rewrites the source and destination addresses of every IPv4 socket CRIU restores.

If a core image lacks a recorded LSM label and the destination has one, the restore is refused with a message to take the checkpoint again.
Labels are recorded only when securityfs was visible to the agent at dump time.

## GPU restore

Which path runs is decided by the manifest, not by the destination:

| Manifest | What runs | Who calls the CUDA driver |
| --- | --- | --- |
| `customStorage: false` | `cuda-checkpoint-helper --action restore` for every CUDA PID with the device map, then `--action unlock` for every PID | `nsrestore`, inside the container |
| `customStorage: true` | One `restore_gpu` request to PageBroker with a pidfd per process | PageBroker, which streams the `gpu/` files back with `O_DIRECT` and unlocks |

On the native path, device memory was in the process's host memory, which CRIU has already restored, and the driver copies it back to the device.
On the CustomStorage path, device memory was never in the process image, and PageBroker reads it from the original artifact even in staged mode.
Either way, the device map tells the driver which destination GPU stands in for each source GPU.

CRIU's own cleanup is deferred until after the unlock, to keep the window in which CUDA is locked as short as possible.

## Completion

Back in the agent, after `nsrestore` returns:

1. The restore source mount is removed and PageBroker's transaction is committed, which releases the pin or the staging directory. The bundle mount is removed. The CuInterpose mount is kept, since restored processes load from it later.
2. The restored PID is checked to be alive and not a zombie.
3. `restore-complete`, containing `pid=<restored PID>`, is written atomically into the container's control directory through `openat2` with symlink resolution disabled.

The proxy sees the file, reads the PID, logs the restored command line, and starts following the process.
If the pod has the startup gate, `cat /snapshot-control/restore-complete` now succeeds and the kubelet begins running the workload's own readiness probe.
The workload, which has been waiting on `restore-complete` as the [workload contract](../workload-contract.md) requires, resumes.

The agent logs a timing summary with these phases: `pagebroker_stage`, `pagebroker_mount`, `pagebroker_commit`, `gpu_device_map`, `overlay_capture`, `criu_prepare`, `criu_restore`, and `cuda_restore`.

## Outcomes and reporting

Each worker returns success, a refusal, or an error.
The controller aggregates them into the `nvidia.com/Restored` pod condition and one event:

| Workers | Condition | Reason |
| --- | --- | --- |
| All succeeded | `True` | `RestoreSucceeded` |
| Some succeeded | `False` | `RestorePartiallySucceeded` |
| Only refusals | `False` | `RestoreIncompatible` |
| Any error | `False` | `RestoreFailed` |
| Any still pending | `False` | `RestoreInProgress`, requeued |

The condition is written with server-side apply under a dedicated field manager, so the agent never touches any other part of the pod's status.
A terminal outcome removes the finalizer.
`podcontract.ClassifyRestoreOutcome` turns the condition into a small enum for integrators.
Dependency-wait reasons such as `SnapshotPending` classify as `Unknown` on purpose, so an integrator can apply its own policy to them.

On an error the controller writes `restore-failed` into the control directory, then kills the container with `SIGKILL`.
The proxy, if it is still running, exits 112.
With `restartPolicy: Never` the pod ends there.
With any other policy the container restarts, the proxy finds the sentinel, exits 112 again, and the pod enters `CrashLoopBackOff`, which is why restore pods are expected to use `Never`.

For a CustomStorage restore that fails, `restore-failed` is written before the abort, because PageBroker must drain GPU work before the container can be stopped, and the container is terminated only once PageBroker confirms.

Events the agent emits on the pod, all with reporting controller `snapshot`:

| Event | Type | When |
| --- | --- | --- |
| `SnapshotPending`, `ContentPending`, `ArtifactPending`, `PodIPPending` | Normal | A dependency is not ready yet |
| `RestoreCompatibilityUnchecked` | Warning | The manifest could not be read at preflight, so the gates did not run |
| `RestoreRequested` | Normal | A destination's restore started |
| `RestoreWorkerFailed` | Warning | A destination's restore failed, with the error text |
| `RestoreCleanupFailed` | Warning | The restore succeeded but a cleanup step did not |
| `RestoreSucceeded`, `RestorePartiallySucceeded`, `RestoreIncompatible`, `RestoreFailed` | Normal or Warning | The terminal outcome, same text as the condition |
| `RestoreAlreadyCompleted`, `RestoreAlreadyPartiallySucceeded`, `RestoreAlreadyFailed` | Normal or Warning | The pod was already terminal when the agent saw it |
| `RestoreStatusUpdateFailed`, `RestoreFinalizerUpdateFailed` | Warning | The agent could not write the pod |

A refusal's message starts with `Refused restore; this node cannot run the checkpoint:` followed by one `<check>: source <value>, target <value>` entry per failed rule.

## Multi-GPU hooks

Where a CuInterpose restore differs from a single-GPU one:

- The library hashes in the bundle must match the manifest before anything else happens.
- `/tmp/snapshot-cuda` is mounted into the container and left mounted after success.
- The coordinator binary is opened by file descriptor before CRIU, like the helper.
- `cuinterpose-coordinator --restore` runs after the GPU unlock and before `restore-complete` is written.

[Multi-GPU support](multi-gpu.md#restore) explains what the coordinator rebuilds.

## Invariants

- A pod gets one restore. The sentinels persist in the `emptyDir`, the proxy never deletes them, and a terminal condition is never replayed. The one exception is an incompatible refusal reopened by the skip annotation.
- The Pod object is never mutated by NRI. Only the OCI arguments change, and an NRI error fails creation rather than falling back to the image's command.
- The proxy is mandatory wherever NRI can provide it. A container whose PID 1 is not the proxy fails the restore.
- Every path the agent places in a container comes from a compiled-in allow list, and the artifact mount is `noexec`.
- Nothing executable or configurable comes from the checkpoint. CRIU, its plugins, `tar`, and the CRIU configuration all come from the agent.
- Restore settings are the dump-time settings from the manifest, except for the destination-specific options listed above.
- Both compatibility gates raise the same error, render reasons the same way, and treat an unknown value as a pass.
- Placement is fixed before the restore starts: cgroup, LSM label, GPUs, and the device map all come from the destination container as it was created.
- CUDA stays locked until CRIU and the GPU restore have both finished.
- Status ownership is narrow: one field manager, one condition, written by one coordinator per pass.
