<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# CUDA checkpoint helper

This command supports driver-managed GPU checkpoint and restore. It also queries
process state and the restore thread. It is built from `main.c` and does not call
`cuInit`. The PageBroker GPU engine handles
CustomStorage operations and GPU data transfers.

```sh
cuda-checkpoint-helper --get-state --pid <pid>
cuda-checkpoint-helper --get-restore-tid --pid <pid>
cuda-checkpoint-helper --action checkpoint --pid 1234
```

The accepted actions are `lock`, `checkpoint`, `restore`, and `unlock`.

`--job-file <path>` sets `CUDA_CHECKPOINT_JOB_FILE` for the driver operation.
`--timeout <ms>` applies to lock operations. Restore accepts
`--device-map <uuids>` for device remapping. See `main.c` for argument validation.

## Build

Build with a C compiler and the matching CUDA headers and driver stub:

```sh
make -C agent/cmd/cuda-checkpoint-helper helper CUDA_ROOT=/path/to/cuda
```

The agent image builds this executable and includes it in the restore injection
bundle. The host NVIDIA driver supplies runtime `libcuda`. The helper has no
daemon, session protocol, transfer ring, NIXL, protobuf, or payload checksum
dependency.
