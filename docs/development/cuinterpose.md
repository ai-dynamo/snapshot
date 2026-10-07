<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# CuInterpose

CuInterpose adds opt-in checkpoint and restore for CUDA memory shared between
processes on one node: POSIX-exported VMM, supported synchronous memory IPC, and
multicast objects. It preserves application virtual addresses and sharing by
saving one copy of shared backing in host memory captured by CRIU, then
reconstructing shared resources after native CUDA restore. Private allocations
stay on the native CUDA checkpoint path.

See the [CUDA shared-memory guide](../guides/cuda-shared-memory.md) for
ordinary Pod and SnapshotJob activation, engine recipes, matching agent images,
and deployment gotchas. The public opt-in is
`nvidia.com/cuda-shared-memory-support: "enabled"` on the source Pod, and the
agent reads only this annotation. SnapshotJob admission rejects invalid annotation
values, and the operator adds the libraries and launcher for enabled sources. An
ordinary Pod adds them itself in a read-only mount at `/tmp/snapshot-cuda` and
starts its target `command` with
`/tmp/snapshot-cuda/cuinterpose-launch --library /tmp/snapshot-cuda/libcuinterpose.so --`.
Capture preflight rejects an opted-in target without that prefix without waiting
for readiness, and capture rejects a writable delivery mount.

Applications must finish all CUDA calls and GPU work, keep a fixed group of fully
interposed peers, keep library files stable, and remain parked until restore
completes. Capture verifies the mapped frontend and both delivered library hashes
in every CUDA participant. Only processes with a mapped core must expose a
coordinator endpoint, at `/tmp/cuinterpose-<namespace-pid>.sock`, so `/tmp` must be
writable. A frontend-only
process can hold native CUDA state without having initialized the shim runtime.
Removing the annotation does not disable an active shim.

The manifest records library identities in `cuinterpose.libraries`, keyed by file
name with a `sha256` field, and coordinator namespace PIDs in `cuinterpose.pids`, an explicit
subset of `cudaRestore.pids`. Native CUDA checkpoint and restore always retain the full
`cudaRestore.pids` list. An empty coordinator subset skips the coordinator but still
requires matching libraries on restore. Existing native CUDA jobfiles remain
supported. A nonempty coordinator subset permits an absent jobfile; an empty
subset keeps the native multi-GPU jobfile requirement.

The [workload contract](../reference/workload-contract.md#cuinterpose-synchronization-and-lifetime) collects the synchronization and lifetime requirements, including the shim's behavior when an application violates them.

Restore requires the captured SHA-256 hashes of both libraries to match the
restore agent's copies, even when compatibility checks are skipped. Use a
matching shim bundle or recreate the checkpoint after a shim upgrade.

Each shared VMM allocation keeps its original creator PID and allocation ID.
For checkpoint, the coordinator selects one existing holder to save and recreate
its backing: the creator if it retains a handle or mapping, otherwise the holder
with the lowest namespace PID. The original creator process must still belong
to the captured group. Existing imports may outlive the creator's local backing
references, but new imports still require those references. A virtual shareable
FD alone does not retain backing.

Memory imported from a process without the shim, such as a GPU Memory Service
server, stays native. The shim tracks its handles and mappings, and inspection
refuses checkpoint until the workload releases all of them.

Shared pinned VMM allocations at DEVICE and HOST_NUMA locations use the selected
holder's host carrier. Device bytes use asynchronous CUDA copies; HOST_NUMA bytes
use CPU copies through a temporary host-accessible VMM alias. Restore preserves
the allocation's NUMA placement separately from the CUDA device used for an
operational context.

Each process's carrier holds the exact full backing of every shared allocation
assigned to it, including regions absent from its application mappings. Its size
is the sum of those backing sizes. Other holders reconnect to that one restored
copy. There is no fixed carrier cap. Budget additional host RAM during capture
and restore, plus CRIU image I/O and storage for the carrier contents. Restore
must recover these host pages and copy their contents into the recreated
allocations before serving can resume.
Larger carriers therefore add work to both CRIU restore and allocation replay.

In a measured vLLM GLM 5.2 capture with the KV cache asleep, the carrier held
1,914 MiB per worker, or 14.95 GiB across eight workers. This is a workload
example, not an upper bound. A large shared KV cache left awake can make the
carrier much larger, increasing host memory, checkpoint storage and restore
copying costs with the total shared backing size.

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
