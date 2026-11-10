<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Prerequisites

Every Snapshot installation needs the baseline prerequisites. Some features need
more than the baseline, such as a newer driver or container runtime. They are
listed per feature. If a feature is not listed, the baseline is all it needs.

## Baseline Prerequisites

- A Kubernetes cluster with NVIDIA GPU nodes on x86_64 or arm64, running Linux
  kernel 5.13 or newer.
- containerd or CRI-O as the container runtime. The chart defaults to
  containerd. For CRI-O and OpenShift, see [Installation](installation.md).
- [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) 26.3 or newer,
  with MIG disabled.
- NVIDIA kernel-mode driver 580 or newer. A restore on a node with an older
  kernel-mode driver is refused with the `driver-minimum` check.
- A `ReadWriteMany` storage class, or an existing `ReadWriteMany` claim, for the
  snapshot volume. See
  [Configure Storage](../operate/configure-storage.md).
- A cluster that allows a privileged DaemonSet with `hostPID`, `hostIPC`, and
  `hostNetwork` for the node agent. See
  [Allow the Privileged Node Agent](installation.md#allow-the-privileged-node-agent).
- The [Helm](https://helm.sh/docs/intro/install) CLI.

## Prerequisites by Feature

### S3 Snapshot Store

Requires a Snapshot release newer than v0.2.0, with the chart, agent, and
PageBroker images from the same release. The v0.2.0 images do not include the
S3 backend. The chart ships the agent and PageBroker as a pair, so
`image.agent.tag` selects both. With S3, you do not need a `ReadWriteMany`
storage class. See [Other Backends](../operate/configure-storage.md#other-backends).

### Multi-GPU Workloads

Requires the baseline if the workload holds no multicast groups. Otherwise,
requires CUDA user-mode driver (`libcuda.so`) 615 or newer.

The 580 user-mode driver cannot restore a process that holds
[multicast groups](../design/multi-gpu/README.md#multicast-mappings),
for example through NCCL NVLS. The restore fails with
`CUDA_ERROR_INVALID_DEVICE`. To use a 615 or newer user-mode driver with an
older kernel-mode driver, point `LD_LIBRARY_PATH` at the directory that contains
its `libcuda.so` in both the workload and PageBroker. See
[Multi-GPU Workloads](../reference/workload-contract.md#multi-gpu-workloads).

### Restore Proxy

Requires a container runtime with NRI v0.10 or newer, enabled: containerd 2.2 or
newer.

On a node without NRI, or with an older runtime, the agent restores without the
proxy. Restore pods then need an inert command, and the container does not
follow the restored workload. See
[Nodes Without the Restore Proxy](../reference/restore-pod-contract.md#nodes-without-the-restore-proxy).

### Pod Groups

Requires Kubernetes 1.32 or newer, or Kubernetes 1.30 or 1.31 with the
`ServiceAccountTokenPodNodeInfo` feature gate on, as it is by default. Pod
groups are alpha and off by default. See
[Pod Groups](../reference/helm-values.md#pod-groups).

The pod group coordinator admits an agent only by a ServiceAccount token that
names the agent pod's node. On a cluster whose tokens do not name it, the
coordinator refuses every agent and logs why.
