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
| Baseline (all workloads) | [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) 26.3+, CUDA driver 580+, MIG disabled | See [Prerequisites](../../README.md#prerequisites). |

No shipped feature currently raises this floor. See
[Limitations](../limitations.md) for functionality still on the roadmap; once
a roadmap feature ships with its own version floor, it gets a row here.
