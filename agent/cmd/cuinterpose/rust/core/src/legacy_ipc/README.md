<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
SPDX-License-Identifier: Apache-2.0
-->

# Legacy memory IPC adapter

This removable compatibility module replaces legacy CUDA memory IPC with tracked POSIX VMM sharing. It never invokes native `cuIpcGetMemHandle`, `cuIpcOpenMemHandle`, or `cuIpcCloseMemHandle`, so these memory dependencies do not require CUDA's checkpoint jobfile.

`cuMemAlloc_v2` creates POSIX-capable VMM backing in the current device context, reserves a granularity-aligned virtual range, and records the application's requested size separately from the rounded extent. `cuMemGetAddressRange_v2` reports the requested range. `cuMemFree_v2` synchronizes the owning context before removing a managed mapping; allocations not owned by this module are freed by the driver.

An IPC export marks the allocation shared and publishes its backing in the existing allocation-ID keyed export cache. CUDA's opaque 64-byte handle carries an inline versioned ticket: original participant ID, allocation ID, namespace PID, requested size, and mapped extent. It is not an OS descriptor. An importer reconstructs the creator endpoint in the shared control directory and uses the existing peer protocol to fetch a fresh POSIX descriptor. Identity checking and descriptor ownership remain in the common ticket and export-cache implementation.

Repeated opens in the same context return the same address and increment a local open count; only the final close removes that mapping. Importers must close before the creator frees the allocation, as required by CUDA's legacy IPC contract. Foreign/native IPC handles are rejected instead of silently creating sharing that the shim cannot reconstruct.

## Checkpoint ownership

The adapter contributes ordinary allocation and mapping records to the existing VMM lifecycle. With host-carrier storage, never-exported allocations remain native-owned; shared creators save canonical bytes in host carriers and importers save no duplicate content. Prepare removes shared backing and mappings but keeps address reservations and adapter metadata. Restore rebuilds the allocation at the same addresses, preserving application pointers and IPC tickets. Native CustomStorage can transfer the remaining private allocation contents through PageBroker.

The adapter itself does not transfer checkpoint bytes, coordinate phases, or call PageBroker.

## Supported scope and removal

The initial scope is Linux/amd64, synchronous device `cuMemAlloc`, and memory IPC between fully interposed processes using one owning context per allocation/import. VMM access is granted to the current device; general native malloc peer-access emulation across multiple contexts is not provided. Managed, pitched, asynchronous pool allocations, pool IPC, and IPC events are not implemented here. This module alone does not establish that an arbitrary application is safe to launch without a jobfile.

The module and its tests are isolated here conceptually; the external integration points are the frontend/Core ABI entries, the typed driver declarations, and `State::mallocs`. The common `import_ticket` function is shared with POSIX ticket imports. When upstream native IPC and CustomStorage compose, removing the adapter does not require changing host carriers, multicast, the peer protocol, or PageBroker.
