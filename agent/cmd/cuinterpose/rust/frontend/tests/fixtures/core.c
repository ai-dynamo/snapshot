// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// An independent core boundary fixture, NOT the Rust checkpoint core. Returning
// host-resolved driver functions isolates front-end loading and forwarding from
// checkpoint state. The C layout also checks the cross-library table ABI.
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <dlfcn.h>
#include <assert.h>
#include "cuda.h"

typedef void *(*resolve_fn)(const char *);
struct host_api {
    uint32_t version, size;
    resolve_fn resolve;
    int (*enter)(void);
    void (*leave)(void);
    int origin_pid;
};
struct core_api {
    uint32_t version, size;
    void (*debug_stats)(void *);
    void (*fork_prepare)(void);
    void (*fork_parent)(void);
    void (*fork_child)(void);
    int (*ensure_ready)(void);
    create_fn cuMemCreate;
    int (*cuMemRelease)(uint64_t);
    int (*cuMemRetainAllocationHandle)(uint64_t *, void *);
    int (*cuMemMap)(uint64_t, size_t, size_t, uint64_t, uint64_t);
    int (*cuMemUnmap)(uint64_t, size_t);
    int (*cuMemSetAccess)(uint64_t, size_t, const void *, size_t);
    int (*cuMemExportToShareableHandle)(void *, uint64_t, unsigned, uint64_t);
    int (*cuMemImportFromShareableHandle)(uint64_t *, void *, unsigned);
    int (*cuMemGetAllocationPropertiesFromHandle)(void *, uint64_t);
    int (*cuMulticastCreate)(uint64_t *, const void *);
    int (*cuMulticastAddDevice)(uint64_t, int);
    bind_v1 cuMulticastBindMem;
    bind_v2 cuMulticastBindMem_v2;
    bind_addr_v1 cuMulticastBindAddr;
    bind_addr_v2 cuMulticastBindAddr_v2;
    int (*cuMulticastGetGranularity)(size_t *, const void *, unsigned);
    int (*cuMulticastUnbind)(uint64_t, int, size_t, size_t);
};

static struct core_api api;
static create_fn next_create;
static bind_v1 next_bind;
static unsigned counts[2];
const unsigned *fixture_core_counts(void) { return counts; }
static int create(uint64_t *out, size_t size, const void *prop, uint64_t flags) {
    ++counts[0];
    return next_create(out, size, prop, flags);
}
static int bind_memory(uint64_t handle, size_t offset, uint64_t member,
                       size_t member_offset, size_t size, uint64_t flags) {
    ++counts[1];
    return next_bind(handle, offset, member, member_offset, size, flags);
}
__attribute__((constructor)) static void reenter_frontend(void) {
    if (!getenv("CUINTERPOSE_TEST_REENTER_CORE"))
        return;
    create_fn create = (create_fn)dlsym(RTLD_DEFAULT, "cuMemCreate");
    uint64_t handle = 0;
    assert(create && create(&handle, 4096, NULL, 0) == 3);
}
static void fork_hook(void) {}
static void debug_stats(void *output) { (void)output; }
static int ensure_ready(void) {
    return getenv("CUINTERPOSE_TEST_READY_FAILURE") ? 3 : 0;
}
// Every table field is a valid non-null function pointer, even those not
// exercised by this focused fixture. These stubs have the declared ABI.
static int release(uint64_t handle) { (void)handle; return 3; }
static int retain(uint64_t *out, void *address) { (void)out; (void)address; return 3; }
static int unmap(uint64_t address, size_t size) { (void)address; (void)size; return 3; }
static int access_memory(uint64_t address, size_t size, const void *access, size_t count) {
    (void)address; (void)size; (void)access; (void)count; return 3;
}
static int export_memory(void *out, uint64_t handle, unsigned kind, uint64_t flags) {
    (void)out; (void)handle; (void)kind; (void)flags; return 3;
}
static int import_memory(uint64_t *out, void *fd, unsigned kind) {
    (void)out; (void)fd; (void)kind; return 3;
}
static int properties(void *out, uint64_t handle) { (void)out; (void)handle; return 3; }
static int multicast_create(uint64_t *out, const void *prop) { (void)out; (void)prop; return 3; }
static int add_device(uint64_t handle, int device) { (void)handle; (void)device; return 3; }
static int granularity(size_t *out, const void *prop, unsigned flags) {
    (void)out; (void)prop; (void)flags; return 3;
}
static int unbind(uint64_t handle, int device, size_t offset, size_t size) {
    (void)handle; (void)device; (void)offset; (void)size; return 3;
}

int cuinterpose_core_init(const struct host_api *host, const struct core_api **output) {
    if (!host || !output || host->version != 4 || host->size != sizeof(*host))
        return 1;
    api.version = 4;
    api.size = sizeof(api);
    api.debug_stats = debug_stats;
    api.fork_prepare = fork_hook;
    api.fork_parent = fork_hook;
    api.fork_child = fork_hook;
    api.ensure_ready = ensure_ready;
    api.cuMemRelease = release;
    api.cuMemRetainAllocationHandle = retain;
    api.cuMemUnmap = unmap;
    api.cuMemSetAccess = access_memory;
    api.cuMemExportToShareableHandle = export_memory;
    api.cuMemImportFromShareableHandle = import_memory;
    api.cuMemGetAllocationPropertiesFromHandle = properties;
    api.cuMulticastCreate = multicast_create;
    api.cuMulticastAddDevice = add_device;
    api.cuMulticastGetGranularity = granularity;
    api.cuMulticastUnbind = unbind;
    next_create = (create_fn)host->resolve("cuMemCreate");
    api.cuMemCreate = create;
    api.cuMemMap = host->resolve("cuMemMap");
    next_bind = (bind_v1)host->resolve("cuMulticastBindMem");
    api.cuMulticastBindMem = bind_memory;
    api.cuMulticastBindMem_v2 = (bind_v2)host->resolve("cuMulticastBindMem_v2");
    api.cuMulticastBindAddr = (bind_addr_v1)host->resolve("cuMulticastBindAddr");
    api.cuMulticastBindAddr_v2 = (bind_addr_v2)host->resolve("cuMulticastBindAddr_v2");
#ifdef BAD_CORE_ABI
    api.version = 999;
#endif
    *output = &api;
    // Observed only by this process, to prove initialization is lazy.
    return setenv("CUINTERPOSE_TEST_CORE_INITIALIZED", "1", 1) == 0 ? 0 : 999;
}
