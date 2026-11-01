<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Overview

Snapshot provides the checkpoint and restore primitives for GPU pods.
Orchestration — which pods to checkpoint, when, and how the checkpoints are
restored — is left to the systems that integrate it.

This section is for you if you have:

- **Your own framework or image**, not one of the supported frameworks (vLLM,
  SGLang, TensorRT-LLM).
- **Your own system that checkpoints and restores pods**, such as a Kubernetes
  controller, operator, or serving platform.

Read [Checkpoint and Restore](../guides/README.md) first. It explains the
resources and the flow. This section covers what you need on top of it.

The stages are the same as in [Checkpoint and Restore](../guides/README.md). This
table says what each one means when you bring your own pod.

| Stage | What it means for you | Read |
|-------|-----------------------|------|
| 1. Prepare the workload | Make your own framework or image ready to be checkpointed and restored. | [Use Your Own Framework or Image](use-your-own-framework.md), [Workload Contract](workload-contract.md) |
| 2. Checkpoint a running pod | Give Snapshot a pod to checkpoint: your own source pod for a `PodSnapshot`, or your own pod template for a `SnapshotJob`. | `PodSnapshot`: [Pod Requirements](workload-contract.md#pod-requirements). `SnapshotJob`: [Write Your Own `SnapshotJob`](write-your-own-snapshotjob.md) |
| 3. Restore a new pod from the snapshot | Create a restore pod that names the snapshot. | [Restore Pod Contract](restore-pod-contract.md) |
