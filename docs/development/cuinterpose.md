<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# CuInterpose

CuInterpose adds opt-in checkpoint and restore for CUDA memory shared between
processes on one node: POSIX-exported VMM, supported synchronous memory IPC, and
multicast objects. It preserves application virtual addresses and sharing by
saving shared creator bytes in host memory captured by CRIU, then reconstructing
shared resources after native CUDA restore. Private allocations stay on the
native CUDA checkpoint path.

Annotate the workload Pod template to install and preload the shim:

```yaml
metadata:
  annotations:
    nvidia.com/cuinterpose-enabled: "true"
```

Applications must finish all CUDA calls and GPU work, keep a fixed group of fully
interposed peers, and remain parked until restore completes. Workload commands
are preserved. Existing native CUDA jobfiles remain supported; opt-in permits
one to be absent. Use matching frontend, backend, and coordinator artifacts.

See [SNEP-295](../proposals/295-cuinterpose/README.md) for the motivation, supported
scope, component responsibilities, interception and sharing protocols,
capture/restore ordering, failure behavior, security, and validation plan.
See the [Rust component README](../../agent/cmd/cuinterpose/rust/README.md) for
build and test commands. [Issue #295](https://github.com/ai-dynamo/snapshot/issues/295)
tracks the implementation; current physical-GPU and cross-node qualification
remain outstanding.
