// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include <core_abi.h>
#include <assert.h>
#include <dlfcn.h>
#include <stdio.h>
#include <string.h>

static void *driver;
static int published, lookups;
static void *resolve(const char *name) {
    assert(!published && "driver lookup after runtime publication can acquire the loader under STATE");
    ++lookups;
    if (strcmp(name, "cuMemGetAllocationGranularity") == 0) return NULL;
    return dlsym(driver, name);
}
int main(int argc, char **argv) {
    assert(argc == 3);
    driver = dlopen(argv[2], RTLD_NOW | RTLD_LOCAL);
    void *core = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    assert(driver && core);
    CUresult (*initialize)(const struct FrontendAbi *, const struct BackendAbi **) =
        dlsym(core, "cuinterpose_core_init");
    assert(initialize);
    struct FrontendAbi frontend = {ABI_VERSION, sizeof(frontend), resolve};
    const struct BackendAbi *backend = NULL;
    assert(initialize(&frontend, &backend) == CUDA_SUCCESS);
    assert(lookups == 0);
    assert(backend->ensure_cuinterpose_initialized() == CUDA_SUCCESS);
    assert(lookups > 0);
    published = 1;
    CUmemAllocationProp properties = {0};
    properties.type = CU_MEM_ALLOCATION_TYPE_PINNED;
    properties.location.type = CU_MEM_LOCATION_TYPE_DEVICE;
    properties.requestedHandleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
    CUmemGenericAllocationHandle handle;
    assert(backend->cuMemCreate(&handle, 4096, &properties, 0) == CUDA_SUCCESS);
    CUmemAllocationProp actual = {0};
    assert(backend->cuMemGetAllocationPropertiesFromHandle(&actual, handle) == CUDA_SUCCESS);
    size_t granularity;
    assert(backend->cuMemGetAllocationGranularity(&granularity, &properties,
        CU_MEM_ALLOC_GRANULARITY_MINIMUM) == CUDA_ERROR_NOT_INITIALIZED);
    assert(backend->cuMemRelease(handle) == CUDA_SUCCESS);
    puts("PASS driver symbol cache: no resolver calls under runtime state; missing optional symbol fails on use");
}
