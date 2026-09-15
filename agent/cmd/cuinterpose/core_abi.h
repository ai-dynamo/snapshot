/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * SPDX-License-Identifier: Apache-2.0
 */
#pragma once
#include <stddef.h>
#include <stdint.h>

#define ABI_VERSION 5
#define CUDA_VERSION 13010
struct Location { int32_t kind, id; };
struct AllocationFlags { uint8_t compression, rdma; uint16_t usage; uint8_t reserved[4]; };
struct AllocationProp {
    int32_t kind;
    uint32_t handle_types;
    struct Location location;
    void *win32_metadata;
    struct AllocationFlags flags;
};
struct Access { struct Location location; uint32_t flags; };
struct MulticastProp { uint32_t devices; size_t size; uint64_t handle_types, flags; };
struct DebugStats {
    uint64_t allocations, handles, mappings, multicasts, cached_exports;
    uint64_t live_raw_imports, unsupported_exportable_creations;
    uint32_t phase;
};
struct Host {
    uint32_t version, size;
    void *(*resolve)(const char *);
    int32_t origin_pid;
};

/* One signature inventory defines the C frontend exports, C++ callbacks, and
 * private table. No C++ object, exception, or ownership crosses this interface. */
#define CUINTERPOSE_MEMORY_API(X) \
 X(cuMemCreate, (uint64_t *out, size_t size, const struct AllocationProp *prop, uint64_t flags), (out, size, prop, flags)) \
 X(cuMemRelease, (uint64_t handle), (handle)) \
 X(cuMemRetainAllocationHandle, (uint64_t *out, void *address), (out, address)) \
 X(cuMemMap, (uint64_t address, size_t size, size_t offset, uint64_t handle, uint64_t flags), (address, size, offset, handle, flags)) \
 X(cuMemUnmap, (uint64_t address, size_t size), (address, size)) \
 X(cuMemSetAccess, (uint64_t address, size_t size, const struct Access *access, size_t count), (address, size, access, count)) \
 X(cuMemExportToShareableHandle, (void *out, uint64_t handle, uint32_t kind, uint64_t flags), (out, handle, kind, flags)) \
 X(cuMemImportFromShareableHandle, (uint64_t *out, void *fd, uint32_t kind), (out, fd, kind)) \
 X(cuMemGetAllocationPropertiesFromHandle, (struct AllocationProp *out, uint64_t handle), (out, handle)) \
 X(cuMulticastCreate, (uint64_t *out, const struct MulticastProp *prop), (out, prop)) \
 X(cuMulticastAddDevice, (uint64_t handle, int32_t device), (handle, device)) \
 X(cuMulticastBindMem, (uint64_t handle, size_t offset, uint64_t member, size_t member_offset, size_t size, uint64_t flags), (handle, offset, member, member_offset, size, flags)) \
 X(cuMulticastBindMem_v2, (uint64_t handle, int32_t device, size_t offset, uint64_t member, size_t member_offset, size_t size, uint64_t flags), (handle, device, offset, member, member_offset, size, flags)) \
 X(cuMulticastBindAddr, (uint64_t handle, size_t offset, uint64_t address, size_t size, uint64_t flags), (handle, offset, address, size, flags)) \
 X(cuMulticastBindAddr_v2, (uint64_t handle, int32_t device, size_t offset, uint64_t address, size_t size, uint64_t flags), (handle, device, offset, address, size, flags)) \
 X(cuMulticastGetGranularity, (size_t *out, const struct MulticastProp *prop, uint32_t flags), (out, prop, flags)) \
 X(cuMulticastUnbind, (uint64_t handle, int32_t device, size_t offset, size_t size), (handle, device, offset, size))

#define CALLBACK(name, parameters, arguments) int (*name) parameters;
struct Core {
    uint32_t version, size;
    void (*debug_stats)(struct DebugStats *);
    void (*fork_prepare)(void), (*fork_parent)(void), (*fork_child)(void);
    int (*ensure_ready)(void);
    CUINTERPOSE_MEMORY_API(CALLBACK)
};
#undef CALLBACK
#ifdef __cplusplus
static_assert(sizeof(Host) == 24 && sizeof(Core) == 184 && sizeof(DebugStats) == 64);
static_assert(sizeof(AllocationProp) == 32 && sizeof(Access) == 12 && sizeof(MulticastProp) == 32);
static_assert(offsetof(Core, cuMemCreate) == 48 && offsetof(Core, cuMulticastUnbind) == 176);
#else
_Static_assert(sizeof(struct Host) == 24 && sizeof(struct Core) == 184, "private ABI layout");
#endif
