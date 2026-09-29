<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Roadmap

## 1. Scheduling & Lifecycle

Let Snapshot drive checkpoint/restore on its own, rather than only on request:

- **Automatic checkpoint/restore**: Trigger checkpoint and restore automatically.
- **Preemption and eviction recovery**: Automatically checkpoint a workload before it's preempted or evicted, so it can resume rather than restart cold.

## 2. Scale

Grow from a single GPU to multi-GPU, multi-node, disaggregated, and mixed hardware:

- **Multi-GPU**: Checkpoint and restore workloads spanning multiple GPUs on one node.
- **Multi-node support**: Extend multi-GPU support across nodes, over NVLink, InfiniBand, RoCE, and other fabrics.
- **Disaggregated deployments**: Support multi-GPU disaggregated (prefill/decode) deployments, including Grove.
- **Heterogeneous GPU support**: Checkpoint on one GPU type and restore onto a different, compatible one.

## 3. Storage & Caching

Broaden where checkpoints can live and speed up how fast they move:

- **Object storage**: Checkpoint to object storage backends.
- **VRAM restore optimization (Streamer)**: Stream checkpoint data directly into VRAM to speed up restore.
- **Multiple storage backends**: Support additional storage backend types beyond PVC, selectable per deployment.
- **ModelExpress P2P restore**: Load model weights peer-to-peer via ModelExpress for vLLM, SGLang, and TRT-LLM, in parallel with restoring the rest of the checkpointed state through the normal restore path.
- **Dedicated caching storage**: Give workloads dedicated caching storage, including sharded NVMe.
- **Pre warm-up nodes**: Cache a checkpoint's data on selected nodes ahead of time, so restore on those nodes starts from local cache instead of the storage backend.
- **Checkpoint optimizations**: Further reduce checkpoint size and time.

## 4. Reliability & Observability

Make checkpoint/restore behavior visible and safe to depend on:

- **Automatic fallback to cold start**: Fall back to a normal cold start if checkpoint/restore fails.
- **Checkpoint/restore metrics**: Expose observability metrics for checkpoint and restore operations.
- **Failover and high-availability restore**: Restore a workload elsewhere automatically after a node failure.

## 5. Workload Types

Extend beyond inference serving to other GPU workload shapes:

- **GPU Instance, training, interactive workloads, and GPU sandboxes**: Support checkpoint/restore for workload types beyond inference serving.
