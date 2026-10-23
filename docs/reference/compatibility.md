<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Compatibility

Snapshot has one general minimum dependency floor, in the baseline row below.
[Prerequisites](../../README.md#prerequisites) lists the other cluster
requirements. Some features need more than that floor — a newer NVIDIA GPU
driver, a newer GPU Operator, or another dependency version. This page is the single place those per-feature floors are
recorded; if a feature isn't listed here, the general floor is all it needs.

## Minimum versions by feature

| Feature | Requires | Notes |
| --- | --- | --- |
| Baseline (all workloads) | [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) 26.3+, NVIDIA kernel-mode driver 580+, CUDA user-mode driver (`libcuda.so`) 615+, MIG disabled | With a kernel-mode driver older than the 615 user-mode requirement, point `LD_LIBRARY_PATH` at a 615 or newer `libcuda.so` in both the workload and PageBroker. |

No shipped feature currently raises this floor. See
[Limitations](../limitations.md) for functionality still on the roadmap; once
a roadmap feature ships with its own version floor, it gets a row here.
