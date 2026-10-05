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

See the [CUDA shared-memory guide](../guides/cuda-shared-memory.md) for
ordinary Pod and SnapshotJob activation, engine recipes, matching agent images,
and deployment gotchas. The public opt-in is
`nvidia.com/cuda-shared-memory-support: "enabled"`.

Applications must finish all CUDA calls and GPU work, keep a fixed group of fully
interposed peers, keep library files stable, and remain parked until restore
completes. Capture checks the libraries and coordinator endpoint in every CUDA
participant before preparation; removing the annotation does not disable an
active shim. Existing native CUDA jobfiles remain supported; verified shim
activation permits one to be absent.

The [workload contract](../reference/workload-contract.md#cuinterpose-synchronization-and-lifetime) collects the synchronization and lifetime requirements, including the shim's behavior when an application violates them.

Restore requires the captured SHA-256 hashes of both libraries to match the
restore agent's copies, even when compatibility checks are skipped. Use a
matching shim bundle or recreate the checkpoint after a shim upgrade. Earlier
draft checkpoints containing `cuinterpose: true` must be recreated.

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

The memory-IPC adapter grants each converted malloc mapping access from its allocating GPU and each IPC import access from its importing GPU. Successful `cuCtxEnablePeerAccess` calls also grant the current GPU access to existing and future converted mappings owned by the peer context. These permissions are recorded and replayed during reconstruction. Making another GPU visible alone does not add a grant.

The adapter assumes cooperating processes, typically one process per GPU. Disabling peer access stops grants for future mappings, while existing mappings retain their device permissions. Context boundaries and peer disable therefore do not provide access isolation for converted mappings. Explicit application-managed VMM mappings retain their own access policy. See the [workload contract](../reference/workload-contract.md#cuinterpose-synchronization-and-lifetime) for context lifetime and synchronization requirements.

Each converted malloc allocation gets its own backing rounded to the device's minimum VMM allocation granularity. There is no pooling or suballocation. A device with a 2 MiB minimum therefore consumes 2 MiB even for a 64 KiB allocation. This can reduce memory available for KV cache and increase allocation-heavy startup costs. Device attributes and granularity are cached, but that does not remove the backing overhead.

CUDA Runtime 11 is unsupported. Runtime driver-entry lookup requires CUDA
Runtime 12.0 or newer and fails closed when the version cannot be verified.

See [SNEP-295](../proposals/295-cuinterpose/README.md) for the motivation, supported
scope, component responsibilities, interception and sharing protocols,
capture/restore ordering, failure behavior, security, and validation plan.
See the [Rust component README](../../agent/cmd/cuinterpose/rust/README.md) for
build and test commands. [Issue #295](https://github.com/ai-dynamo/snapshot/issues/295)
tracks the implementation and qualification.
