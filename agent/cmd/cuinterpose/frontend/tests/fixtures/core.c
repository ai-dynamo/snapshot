// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// A mock backend, NOT the Rust checkpoint core. Returning host-resolved driver
// functions isolates frontend loading and forwarding from checkpoint state.
#include <stddef.h>
#include <stdlib.h>
#include <dlfcn.h>
#include <assert.h>
#include <pthread.h>
#include "core_abi.h"

static struct FrontendAbi registered;
static pthread_mutex_t registration = PTHREAD_MUTEX_INITIALIZER;
static pthread_barrier_t concurrent_registration;
static int concurrent;
__attribute__((constructor)) static void reenter_frontend(void) {
    concurrent = getenv("CUINTERPOSE_TEST_CONCURRENT_CORE") != NULL;
    if (concurrent)
        assert(pthread_barrier_init(&concurrent_registration, NULL, 16) == 0);
    if (!getenv("CUINTERPOSE_TEST_REENTER_CORE"))
        return;
    __typeof__(((struct BackendAbi *)0)->cuMemCreate) create = dlsym(RTLD_DEFAULT, "cuMemCreate");
    CUmemGenericAllocationHandle handle = 0;
    assert(create && create(&handle, 4096, NULL, 0) == 3);
}
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
static CUresult unbind(CUmemGenericAllocationHandle handle, CUdevice device,
                       size_t offset, size_t size) {
    (void)handle; (void)device; (void)offset; (void)size; return 3;
}

// Resolve only when invoking a callback, never while registering the ABI.
#define FORWARD(name, parameters, arguments) \
    static CUresult forward_##name parameters { \
        __typeof__(((struct BackendAbi *)0)->name) function = registered.resolve(#name); \
        assert(function); \
        return function arguments; \
    }
FORWARD(cuMemAlloc_v2, (CUdeviceptr *out, size_t size), (out, size))
FORWARD(cuMemFree_v2, (CUdeviceptr address), (address))
FORWARD(cuMemGetAddressRange_v2, (CUdeviceptr *base, size_t *size, CUdeviceptr address), (base, size, address))
FORWARD(cuIpcGetMemHandle, (CUipcMemHandle *out, CUdeviceptr address), (out, address))
#undef cuIpcOpenMemHandle
FORWARD(cuIpcOpenMemHandle, (CUdeviceptr *out, CUipcMemHandle handle, unsigned flags), (out, handle, flags))
FORWARD(cuIpcOpenMemHandle_v2, (CUdeviceptr *out, CUipcMemHandle handle, unsigned flags), (out, handle, flags))
FORWARD(cuIpcCloseMemHandle, (CUdeviceptr address), (address))
FORWARD(cuMemCreate, (CUmemGenericAllocationHandle *out, size_t size, const CUmemAllocationProp *prop, unsigned long long flags), (out, size, prop, flags))
FORWARD(cuMemMap, (CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle, unsigned long long flags), (address, size, offset, handle, flags))
FORWARD(cuMulticastBindMem, (CUmemGenericAllocationHandle handle, size_t offset, CUmemGenericAllocationHandle member, size_t member_offset, size_t size, unsigned long long flags), (handle, offset, member, member_offset, size, flags))
FORWARD(cuMulticastBindMem_v2, (CUmemGenericAllocationHandle handle, CUdevice device, size_t offset, CUmemGenericAllocationHandle member, size_t member_offset, size_t size, unsigned long long flags), (handle, device, offset, member, member_offset, size, flags))
FORWARD(cuMulticastBindAddr, (CUmemGenericAllocationHandle handle, size_t offset, CUdeviceptr address, size_t size, unsigned long long flags), (handle, offset, address, size, flags))
FORWARD(cuMulticastBindAddr_v2, (CUmemGenericAllocationHandle handle, CUdevice device, size_t offset, CUdeviceptr address, size_t size, unsigned long long flags), (handle, device, offset, address, size, flags))
#undef FORWARD

static const struct BackendAbi api = {
#ifdef BAD_CORE_ABI
    .version = 999,
#else
    .version = ABI_VERSION,
#endif
#ifdef BAD_CORE_SIZE
    .size = 8,
#else
    .size = sizeof(api),
#endif
    .ensure_cuinterpose_initialized = ensure_cuinterpose_initialized,
    .cuMemAlloc_v2 = forward_cuMemAlloc_v2,
    .cuMemFree_v2 = forward_cuMemFree_v2,
    .cuMemGetAddressRange_v2 = forward_cuMemGetAddressRange_v2,
    .cuIpcGetMemHandle = forward_cuIpcGetMemHandle,
    .cuIpcOpenMemHandle = forward_cuIpcOpenMemHandle,
    .cuIpcOpenMemHandle_v2 = forward_cuIpcOpenMemHandle_v2,
    .cuIpcCloseMemHandle = forward_cuIpcCloseMemHandle,
    .cuMemCreate = forward_cuMemCreate,
    .cuMemMap = forward_cuMemMap,
    .cuMulticastBindMem = forward_cuMulticastBindMem,
    .cuMulticastBindMem_v2 = forward_cuMulticastBindMem_v2,
    .cuMulticastBindAddr = forward_cuMulticastBindAddr,
    .cuMulticastBindAddr_v2 = forward_cuMulticastBindAddr_v2,
    .cuMemRelease = release,
    .cuMemRetainAllocationHandle = retain,
    .cuMemUnmap = unmap,
    .cuMemSetAccess = access_memory,
    .cuMemExportToShareableHandle = export_memory,
    .cuMemImportFromShareableHandle = import_memory,
    .cuMemGetAllocationPropertiesFromHandle = properties,
    .cuMulticastCreate = multicast_create,
    .cuMulticastAddDevice = add_device,
    .cuMulticastUnbind = unbind,
};

CUresult cuinterpose_core_init(const struct FrontendAbi *frontend,
                               const struct BackendAbi **output) {
    if (!frontend || !output || frontend->version != ABI_VERSION || frontend->size != sizeof(*frontend))
        return 1;
    if (concurrent) {
        // Hold all cold callers in the handshake before any can publish.
        int result = pthread_barrier_wait(&concurrent_registration);
        assert(result == 0 || result == PTHREAD_BARRIER_SERIAL_THREAD);
    }
    assert(pthread_mutex_lock(&registration) == 0);
    if (!registered.resolve) {
        registered = *frontend;
        // Observed only by this process, to prove registration is lazy.
        assert(setenv("CUINTERPOSE_TEST_CORE_INITIALIZED", "1", 1) == 0);
    }
    int matches = registered.resolve == frontend->resolve;
    assert(pthread_mutex_unlock(&registration) == 0);
    if (!matches)
        return 1;
    *output = &api;
    return CUDA_SUCCESS;
}
