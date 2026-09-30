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

Creators must retain a generic allocation handle or local mapping while their
exported descriptors or imported allocations remain usable; a virtual shareable
FD alone does not retain backing. Shared pinned VMM allocations at DEVICE and
HOST_NUMA locations use the creator's host carrier. Device bytes use asynchronous
CUDA copies; HOST_NUMA bytes use CPU copies through a temporary host-accessible
VMM alias. Restore preserves the allocation's NUMA placement separately from
the CUDA device used for an operational context.

Context destruction/reset and final primary-context release clean up converted
malloc allocations and imported IPC mappings while preserving explicit VMM
allocations. Multicast participants must add and bind their device in the same
process; device ordinals are local to each participant.

CUDA Runtime 11 is unsupported. Runtime driver-entry lookup requires CUDA
Runtime 12.0 or newer and fails closed when the version cannot be verified.
For manual injection, use an absolute `LD_PRELOAD` path so it remains valid
after exec; the operator supplies an absolute path automatically.

See [SNEP-295](../proposals/295-cuinterpose/README.md) for the motivation, supported
scope, component responsibilities, interception and sharing protocols,
capture/restore ordering, failure behavior, security, and validation plan.
See the [Rust component README](../../agent/cmd/cuinterpose/rust/README.md) for
build and test commands. [Issue #295](https://github.com/ai-dynamo/snapshot/issues/295)
tracks the implementation; current physical-GPU and cross-node qualification
remain outstanding.
