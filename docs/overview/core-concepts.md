<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Core Concepts

Snapshot saves the state of a running GPU pod and starts new pods from it. To
use it, you need to know what is saved, what your workload must do to take
part, how you ask for a checkpoint or a restore, where the saved state is kept,
where it can be restored, and how many times a pod can be restored.

## Checkpoint and Restore

To **checkpoint** a pod is to take a snapshot of it. A **snapshot** is the saved
state of a pod: its running process, with CPU and GPU memory. The pod you
checkpoint is the **source pod**. Taking a snapshot terminates the source
process.

To **restore** is to start a new pod from a snapshot. That new pod is the
**restore pod**. It continues from the state that was saved, so it skips model
loading and warm-up.

## The Workload Contract

Snapshot does not checkpoint an arbitrary pod. The workload must cooperate: it
reaches a state that is safe to checkpoint, signals when it does, and resumes
correctly after a restore. The [Workload Contract](../integrate/workload-contract.md)
defines this. The workload and Snapshot signal each other through files in a
shared folder, such as `ready-for-snapshot` and `restore-complete`.

## Resources

You work with Snapshot through Kubernetes resources:

| Resource | What it is |
| --- | --- |
| `PodSnapshot` | Your request to checkpoint a pod, and the handle that a restore points at. |
| `PodSnapshotContent` | The operator's record of the stored snapshot. The operator creates it, never you. |
| `SnapshotJob` | Runs a pod from a template and checkpoints it once it is ready. |

Restore has no resource of its own. You restore by annotating a new pod with
`nvidia.com/restore-from`, set to the name of a `PodSnapshot` in the same
namespace. The [Restore Pod Contract](../integrate/restore-pod-contract.md)
lists what else the restore pod needs. See the
[API Reference](../reference/api.md) for the fields of each resource.

## Two Ways to Checkpoint

You checkpoint a pod in one of two ways. With a `PodSnapshot`, you run the source
pod yourself, wait until it is ready, and ask Snapshot to checkpoint it. With a
`SnapshotJob`, Snapshot runs the source pod from a template, checkpoints it once
it is ready, and deletes it, so every replica, including the first, starts by
restoring. See [Checkpoint a Pod](../guides/checkpoint.md).

## The Snapshot Store

Snapshots are kept in one snapshot store that every node agent reaches. By
default the store is a `ReadWriteMany` volume, the snapshot volume, which the
chart provisions. It can also be an S3-compatible bucket. Workload pods never
mount the store. See [Configure Storage](../operate/configure-storage.md).

## Compatibility

A snapshot restores only on a node that matches the node where it was taken.
Before a restore, Snapshot compares the snapshot with the node: CPU
architecture, kernel version, image digest, CPU and memory limits, mounts, GPU
model and count, and driver version. When something does not match, Snapshot
refuses the restore and names the check that failed. See
[Compatibility Gates](architecture-overview.md#compatibility-gates).

## Multi-GPU Workloads

A workload can use several GPUs on one node. Multi-GPU support is opt-in: you set
the `nvidia.com/snapshot-multi-gpu: enabled` annotation on the source pod.
Multi-node workloads are not supported. See
[Workload Contract](../integrate/workload-contract.md#multi-gpu-workloads).

## One Restore per Pod

A pod gets one restore. If the restored process exits, the pod does not restore
again; create a new pod to restore again. This is why restore pods use
`restartPolicy: Never`. See
[Restore Pod Contract](../integrate/restore-pod-contract.md#one-restore-per-pod).

## Primitives, Not Orchestration

Snapshot provides the checkpoint and restore primitives for GPU pods.
Orchestration — which pods to checkpoint, when, and how the checkpoints are
restored — is left to the systems that integrate it. See
[Integrate](../integrate/overview.md).
