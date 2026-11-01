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
| [S3 checkpoint storage](../operations/storage.md#other-backends) (`storage.type: s3`) | A Snapshot release newer than v0.2.0, with the chart, agent and PageBroker images from the same release | The v0.2.0 images do not include the S3 backend. The chart ships the agent and PageBroker as a pair, so `image.agent.tag` selects both. |
| GPU CustomStorage | CUDA user-mode driver (`libcuda.so`) 615+ / CUDA driver API 13.4+ in the workload and PageBroker | Snapshot selects CustomStorage when PageBroker reports support. Older user-mode drivers use the native checkpoint path. Restoring a CustomStorage snapshot requires CustomStorage support on the destination. See [Check the CustomStorage driver](#check-the-customstorage-driver). |
| [Multi-GPU workloads](workload-contract.md#multi-gpu-workloads) | Baseline, if the workload holds no multicast groups; otherwise CUDA user-mode driver (`libcuda.so`) 615+ | The 580 user-mode driver cannot restore a process that holds [multicast groups](architecture/multi-gpu.md#multicast-mappings), for example through NCCL NVLS; the restore fails with `CUDA_ERROR_INVALID_DEVICE`. To use a 615 or newer user-mode driver with an older kernel-mode driver, point `LD_LIBRARY_PATH` at the directory that contains its `libcuda.so` in both the workload and PageBroker. |

See [Limitations](../limitations.md) for functionality still on the roadmap;
once a roadmap feature ships with its own version floor, it gets a row here.

## Check the CustomStorage driver

Check the user-mode library loaded by the workload and PageBroker on both
the checkpoint and restore nodes. `nvidia-smi` reports the host driver; a
container can load a different `libcuda.so` through `LD_LIBRARY_PATH`.

On the node, inspect each process's loaded library:

```bash
sudo sed -n '/libcuda\.so/s/^[^/]*//p' /proc/<host-pid>/maps | sort -u
```

The output retains ` (deleted)` if the loaded library's file was removed.

Use matching user-mode driver libraries for the workload, node agent's CUDA
helper, and PageBroker. On a compatible older kernel-mode driver, the
`cuda-compat-13-4` package provides the newer libraries. Select them explicitly
with `LD_LIBRARY_PATH` in those containers; installing the package alone does
not select it. See [NVIDIA's forward-compatibility guide](https://docs.nvidia.com/deploy/cuda-compatibility/forward-compatibility.html).
