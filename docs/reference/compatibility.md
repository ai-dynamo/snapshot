<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Compatibility

Snapshot has one general minimum dependency floor, listed in
[Prerequisites](../../README.md#prerequisites). Some features need more than
that floor — a newer NVIDIA GPU driver, a newer GPU Operator, or another
dependency version. This page is the single place those per-feature floors are
recorded; if a feature isn't listed here, the general floor is all it needs.

## Minimum versions by feature

| Feature | Requires | Notes |
| --- | --- | --- |
| Baseline (all workloads) | [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) 26.3+, NVIDIA kernel-mode driver 580+, MIG disabled | See [Prerequisites](../../README.md#prerequisites). A restore on a node with an older kernel-mode driver is refused with the `driver-minimum` check. |
| [Multi-GPU workloads](workload-contract.md#multi-gpu-workloads) | Baseline, if the workload holds no multicast groups; otherwise CUDA user-mode driver (`libcuda.so`) 615+ | The 580 user-mode driver cannot restore a process that holds [multicast groups](architecture/multi-gpu.md#multicast-mappings), for example through NCCL NVLS; the restore fails with `CUDA_ERROR_INVALID_DEVICE`. To use a 615 or newer user-mode driver with an older kernel-mode driver, point `LD_LIBRARY_PATH` at the directory that contains its `libcuda.so` in both the workload and PageBroker. |

See [Limitations](../limitations.md) for functionality still on the roadmap;
once a roadmap feature ships with its own version floor, it gets a row here.
