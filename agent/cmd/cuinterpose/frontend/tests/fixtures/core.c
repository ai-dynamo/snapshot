// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// A mock backend, NOT the Rust checkpoint core. Returning host-resolved driver
// functions isolates frontend loading and forwarding from checkpoint state.
#include <stddef.h>
#include <stdlib.h>
#include <dlfcn.h>
#include <assert.h>
#include "core_abi.h"

static struct BackendAbi api;
__attribute__((constructor)) static void reenter_frontend(void) {
    if (!getenv("CUINTERPOSE_TEST_REENTER_CORE"))
        return;
    __typeof__(api.cuMemCreate) create = (__typeof__(create))dlsym(RTLD_DEFAULT, "cuMemCreate");
    CUmemGenericAllocationHandle handle = 0;
    assert(create && create(&handle, 4096, NULL, 0) == 3);
}
static void fork_hook(void) {}
static CUresult ensure_cuinterpose_initialized(void) {
    return getenv("CUINTERPOSE_TEST_READY_FAILURE") ? 3 : 0;
}
// Every table field is a valid non-null function pointer, even those not
// exercised by this focused fixture. These stubs have the declared ABI.
static CUresult release(CUmemGenericAllocationHandle handle) { (void)handle; return 3; }
static CUresult retain(CUmemGenericAllocationHandle *out, void *address) {
    (void)out; (void)address; return 3;
}
static CUresult unmap(CUdeviceptr address, size_t size) {
    (void)address; (void)size; return 3;
}
static CUresult access_memory(CUdeviceptr address, size_t size,
                              const CUmemAccessDesc *access, size_t count) {
    (void)address; (void)size; (void)access; (void)count; return 3;
}
static CUresult export_memory(void *out, CUmemGenericAllocationHandle handle,
                              CUmemAllocationHandleType kind, unsigned long long flags) {
    (void)out; (void)handle; (void)kind; (void)flags; return 3;
}
static CUresult import_memory(CUmemGenericAllocationHandle *out, void *fd,
                              CUmemAllocationHandleType kind) {
    (void)out; (void)fd; (void)kind; return 3;
}
static CUresult properties(CUmemAllocationProp *out, CUmemGenericAllocationHandle handle) {
    (void)out; (void)handle; return 3;
}
static CUresult multicast_create(CUmemGenericAllocationHandle *out,
                                 const CUmulticastObjectProp *prop) {
    (void)out; (void)prop; return 3;
}
static CUresult add_device(CUmemGenericAllocationHandle handle, CUdevice device) {
    (void)handle; (void)device; return 3;
}
static CUresult granularity(size_t *out, const CUmemAllocationProp *prop,
                            CUmemAllocationGranularity_flags flags) {
    (void)out; (void)prop; (void)flags; return 3;
}
static CUresult multicast_granularity(size_t *out, const CUmulticastObjectProp *prop,
                                      CUmulticastGranularity_flags flags) {
    (void)out; (void)prop; (void)flags; return 3;
}
static CUresult unbind(CUmemGenericAllocationHandle handle, CUdevice device,
                       size_t offset, size_t size) {
    (void)handle; (void)device; (void)offset; (void)size; return 3;
}

CUresult cuinterpose_core_init(const struct FrontendAbi *frontend,
                               const struct BackendAbi **output) {
    if (!frontend || !output || frontend->version != ABI_VERSION || frontend->size != sizeof(*frontend))
        return 1;
    api.version = ABI_VERSION;
    api.size = sizeof(api);
    api.fork_prepare = fork_hook;
    api.fork_parent = fork_hook;
    api.fork_child = fork_hook;
    api.ensure_cuinterpose_initialized = ensure_cuinterpose_initialized;
    api.cuMemAlloc_v2 = frontend->resolve("cuMemAlloc_v2");
    api.cuMemFree_v2 = frontend->resolve("cuMemFree_v2");
    api.cuMemGetAddressRange_v2 = frontend->resolve("cuMemGetAddressRange_v2");
    api.cuIpcGetMemHandle = frontend->resolve("cuIpcGetMemHandle");
    api.cuIpcOpenMemHandle = frontend->resolve("cuIpcOpenMemHandle");
    api.cuIpcOpenMemHandle_v2 = frontend->resolve("cuIpcOpenMemHandle_v2");
    api.cuIpcCloseMemHandle = frontend->resolve("cuIpcCloseMemHandle");
    api.cuMemRelease = release;
    api.cuMemRetainAllocationHandle = retain;
    api.cuMemUnmap = unmap;
    api.cuMemSetAccess = access_memory;
    api.cuMemExportToShareableHandle = export_memory;
    api.cuMemImportFromShareableHandle = import_memory;
    api.cuMemGetAllocationPropertiesFromHandle = properties;
    api.cuMulticastCreate = multicast_create;
    api.cuMulticastAddDevice = add_device;
    api.cuMulticastGetGranularity = multicast_granularity;
    api.cuMulticastUnbind = unbind;
    api.cuMemCreate = (__typeof__(api.cuMemCreate))frontend->resolve("cuMemCreate");
    api.cuMemGetAllocationGranularity = granularity;
    api.cuMemMap = frontend->resolve("cuMemMap");
    api.cuMulticastBindMem =
        (__typeof__(api.cuMulticastBindMem))frontend->resolve("cuMulticastBindMem");
    api.cuMulticastBindMem_v2 =
        (__typeof__(api.cuMulticastBindMem_v2))frontend->resolve("cuMulticastBindMem_v2");
    api.cuMulticastBindAddr =
        (__typeof__(api.cuMulticastBindAddr))frontend->resolve("cuMulticastBindAddr");
    api.cuMulticastBindAddr_v2 =
        (__typeof__(api.cuMulticastBindAddr_v2))frontend->resolve("cuMulticastBindAddr_v2");
#ifdef BAD_CORE_ABI
    api.version = 999;
#endif
#ifdef BAD_CORE_SIZE
    api.size = 8;
#endif
    *output = &api;
    // Observed only by this process, to prove initialization is lazy.
    return setenv("CUINTERPOSE_TEST_CORE_INITIALIZED", "1", 1) == 0 ? 0 : 999;
}
