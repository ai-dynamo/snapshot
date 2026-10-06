// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Import real backing through the core ABI, replacing only the property-query result.
#include "core_abi.h"
#include <assert.h>
#include <dlfcn.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

static void *driver;
static int fault, imports, releases;
static CUmemGenericAllocationHandle imported;
static CUresult (*native_import)(CUmemGenericAllocationHandle *, void *, CUmemAllocationHandleType);
static CUresult (*native_properties)(CUmemAllocationProp *, CUmemGenericAllocationHandle);
static CUresult (*native_release)(CUmemGenericAllocationHandle);

static CUresult import_handle(CUmemGenericAllocationHandle *out, void *fd,
                              CUmemAllocationHandleType kind) {
    CUresult result = native_import(out, fd, kind);
    if (result == CUDA_SUCCESS) {
        imported = *out;
        imports++;
    }
    return result;
}

static CUresult properties(CUmemAllocationProp *out, CUmemGenericAllocationHandle handle) {
    assert(handle == imported);
    if (fault == 1)
        return CUDA_ERROR_INVALID_HANDLE;
    CUresult result = native_properties(out, handle);
    if (result == CUDA_SUCCESS && fault == 2)
        out->type = CU_MEM_ALLOCATION_TYPE_INVALID;
    return result;
}

static CUresult release_handle(CUmemGenericAllocationHandle handle) {
    assert(handle == imported);
    CUresult result = native_release(handle);
    assert(result == CUDA_SUCCESS);
    releases++;
    return result;
}

static void *resolve(const char *name) {
    if (!strcmp(name, "cuMemImportFromShareableHandle"))
        return import_handle;
    if (!strcmp(name, "cuMemGetAllocationPropertiesFromHandle"))
        return properties;
    if (!strcmp(name, "cuMemRelease"))
        return release_handle;
    return dlsym(driver, name);
}

int main(int argc, char **argv) {
    assert(argc == 5);
    int descriptor = atoi(argv[2]);
    size_t size = strtoull(argv[3], NULL, 10);
    fault = atoi(argv[4]);
    assert(descriptor >= 0 && size > 0 && (fault == 1 || fault == 2));
    driver = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
    void *core = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    assert(driver && core);
    native_import = dlsym(driver, "cuMemImportFromShareableHandle");
    native_properties = dlsym(driver, "cuMemGetAllocationPropertiesFromHandle");
    native_release = dlsym(driver, "cuMemRelease");
    assert(native_import && native_properties && native_release);
    CUresult (*init)(unsigned int) = dlsym(driver, "cuInit");
    CUresult (*retain)(CUcontext *, CUdevice) = dlsym(driver, "cuDevicePrimaryCtxRetain");
    CUresult (*set_current)(CUcontext) = dlsym(driver, "cuCtxSetCurrent");
    assert(init && retain && set_current && init(0) == CUDA_SUCCESS);
    CUcontext context;
    assert(retain(&context, 0) == CUDA_SUCCESS && set_current(context) == CUDA_SUCCESS);
    CUresult (*initialize)(const struct FrontendAbi *, const struct BackendAbi **) =
        dlsym(core, "cuinterpose_core_init");
    const struct FrontendAbi frontend = {ABI_VERSION, sizeof(frontend), resolve};
    const struct BackendAbi *backend = NULL;
    assert(initialize && initialize(&frontend, &backend) == CUDA_SUCCESS);
    assert(backend->ensure_cuinterpose_initialized() == CUDA_SUCCESS);

    CUmemGenericAllocationHandle handle = 99;
    CUresult expected = fault == 1 ? CUDA_ERROR_INVALID_HANDLE : CUDA_ERROR_NOT_SUPPORTED;
    assert(backend->cuMemImportFromShareableHandle(
        &handle, (void *)(intptr_t)descriptor, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) == expected);
    assert(handle == 99 && imports == 1 && releases == 1);
    puts("rejected");
    fflush(stdout);
    // The parent inspects this process before allowing a successful retry.
    assert(getchar() == 'x');

    fault = 0;
    assert(backend->cuMemImportFromShareableHandle(
        &handle, (void *)(intptr_t)descriptor, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) == CUDA_SUCCESS);
    assert(imports == 2 && releases == 1 && handle != 99);
    assert(close(descriptor) == 0);
    CUresult (*reserve)(CUdeviceptr *, size_t, size_t, CUdeviceptr, unsigned long long) =
        dlsym(driver, "cuMemAddressReserve");
    CUresult (*copy)(void *, CUdeviceptr, size_t) = dlsym(driver, "cuMemcpyDtoH_v2");
    CUresult (*free_address)(CUdeviceptr, size_t) = dlsym(driver, "cuMemAddressFree");
    assert(reserve && copy && free_address);
    CUdeviceptr address;
    assert(reserve(&address, size, 0, 0, 0) == CUDA_SUCCESS);
    assert(backend->cuMemMap(address, size, 0, handle, 0) == CUDA_SUCCESS);
    CUmemAccessDesc access = {0};
    access.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    access.location.id = 0;
    access.flags = CU_MEM_ACCESS_FLAGS_PROT_READWRITE;
    assert(backend->cuMemSetAccess(address, size, &access, 1) == CUDA_SUCCESS);
    const char contents[] = "rollback import still works";
    char actual[sizeof(contents)];
    assert(copy(actual, address, sizeof(actual)) == CUDA_SUCCESS);
    assert(memcmp(actual, contents, sizeof(contents)) == 0);
    assert(backend->cuMemUnmap(address, size) == CUDA_SUCCESS);
    assert(free_address(address, size) == CUDA_SUCCESS);
    assert(backend->cuMemRelease(handle) == CUDA_SUCCESS && releases == 2);
    assert(backend->cuDevicePrimaryCtxRelease_v2(0) == CUDA_SUCCESS);
    return 0;
}
