<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Compatibility

Snapshot has one general minimum dependency floor, listed in
[Prerequisites](../../README.md#prerequisites). Some features need more than
that floor — a newer NVIDIA GPU driver, a newer GPU Operator, or another
dependency version. This page is the single place those per-feature floors are
recorded. If a feature isn't listed here, the general floor is all it needs.

## Minimum versions by feature

| Feature | Requires | Notes |
| --- | --- | --- |
| Baseline (all workloads) | [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) 26.3+, NVIDIA kernel-mode driver 580+, MIG disabled | See [Prerequisites](../../README.md#prerequisites). A restore on a node with an older kernel-mode driver is refused with the `driver-minimum` check. |
| [S3 checkpoint storage](../operations/storage.md#other-backends) (`storage.type: s3`) | A Snapshot release newer than v0.2.0, with the chart, agent and PageBroker images from the same release | The v0.2.0 images do not include the S3 backend. The chart ships the agent and PageBroker as a pair, so `image.agent.tag` selects both. |
| GPU CustomStorage | CUDA user-mode driver (`libcuda.so`) 615+ / CUDA driver API 13.4+ in the workload and PageBroker | Snapshot selects CustomStorage when PageBroker reports support. The native checkpoint path is used when PageBroker does not report support. An older workload driver does not trigger an automatic fallback. Restoring a CustomStorage snapshot requires CustomStorage support on the destination. See [Check the CUDA user-mode driver](../operations/cuda-driver.md). |
| [Multi-GPU workloads](workload-contract.md#multi-gpu-workloads) | Baseline without multicast groups. CUDA user-mode driver (`libcuda.so`) 595+ with multicast groups | This minimum also applies with cuinterpose. Restore on older drivers can fail with `CUDA_ERROR_INVALID_DEVICE` for [multicast groups](architecture/multi-gpu.md#multicast-mappings), even when cuinterpose removes and rebuilds the multicast resources. For using a newer user-mode driver with a compatible older kernel-mode driver, see [Check the CUDA user-mode driver](../operations/cuda-driver.md). |

See [Limitations](../limitations.md) for functionality still on the roadmap.
Once a roadmap feature ships with its own version floor, it gets a row here.
