// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include <cuda.h>
#include <assert.h>
#include <dlfcn.h>
#include <stdio.h>
#include <string.h>

// The old and new query entry points coexist in the installed driver.
typedef CUresult (*Query)(const char *, void **, int, cuuint64_t);
typedef CUresult (*QueryV2)(const char *, void **, int, cuuint64_t, CUdriverProcAddressQueryResult *);
typedef int (*RuntimeQuery)(const char *, void **, cuuint64_t, int *);
typedef int (*RuntimeVersionQuery)(const char *, void **, unsigned, cuuint64_t, int *);

static __typeof__(&cuMemsetD8) fill;
static __typeof__(&cuMemcpyDtoH) copy_to_host;
static __typeof__(&cuMemFree) free_memory;

static void *symbol(void *handle, const char *name) {
    void *address = dlsym(handle, name);
    if (!address)
        fprintf(stderr, "missing required symbol %s: %s\n", name, dlerror());
    assert(address);
    return address;
}

static void in_shim(void *address, const char *name) {
    Dl_info info;
    assert(dladdr(address, &info));
    assert(strstr(info.dli_fname, "libcuinterpose.so"));
    assert(info.dli_sname && strcmp(info.dli_sname, name) == 0);
}

static void allocate(void *function) {
    CUdeviceptr address = 0;
    CUresult (*alloc)(CUdeviceptr *, size_t) = function;
    assert(alloc(&address, 4096) == CUDA_SUCCESS && address);
    assert(fill(address, 0x7b, 4096) == CUDA_SUCCESS);
    unsigned char value = 0;
    assert(copy_to_host(&value, address, 1) == CUDA_SUCCESS && value == 0x7b);
    assert(free_memory(address) == CUDA_SUCCESS);
}

int main(int argc, char **argv) {
    assert(argc == 2);
    void *driver = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
    assert(driver);
    __typeof__(&cuInit) initialize = symbol(driver, "cuInit");
    if (strcmp(argv[1], "require-gpus") == 0) {
        void *runtime = dlopen("libcudart.so.13", RTLD_NOW | RTLD_LOCAL);
        if (!runtime) {
            fprintf(stderr, "real CUDA frontend tests require libcudart.so.13: %s\n", dlerror());
            return 1;
        }
        CUresult result = initialize(0);
        __typeof__(&cuDeviceGetCount) device_count = symbol(driver, "cuDeviceGetCount");
        int count = 0;
        if (result != CUDA_SUCCESS || device_count(&count) != CUDA_SUCCESS || count < 2) {
            fprintf(stderr, "real CUDA frontend tests require at least two GPUs (cuInit=%d, GPUs=%d)\n", result, count);
            return 1;
        }
        printf("Real CUDA frontend tests: %d GPUs available\n", count);
        dlclose(runtime);
        return 0;
    }
    void *(*original)(void *, const char *) = dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34");
    assert(original);
    if (strcmp(argv[1], "missing-core") == 0) {
        CUresult (*alloc)(CUdeviceptr *, size_t) = symbol(driver, "cuMemAlloc_v2");
        CUdeviceptr output = 42;
        assert(alloc(&output, 4096) == CUDA_ERROR_NOT_INITIALIZED && output == 42);
        assert(initialize(0) == CUDA_ERROR_NOT_INITIALIZED);
        return 0;
    }
    if (strcmp(argv[1], "lookup") == 0) {
        dlerror();
        assert(!dlsym(driver, "not_a_cuda_api") && dlerror());
        void *libc = dlopen("libc.so.6", RTLD_NOW);
        assert(libc);
        assert(!dlsym(libc, "cuMemCreate"));
        assert(symbol(libc, "malloc") == original(libc, "malloc"));
        in_shim(symbol(driver, "cuMemCreate"), "cuMemCreate");
        in_shim(symbol(driver, "cuCtxEnablePeerAccess"), "cuCtxEnablePeerAccess");
        in_shim(symbol(driver, "cuCtxDisablePeerAccess"), "cuCtxDisablePeerAccess");
        assert(symbol(driver, "cuDriverGetVersion") == original(driver, "cuDriverGetVersion"));
        dlclose(libc);
        return 0;
    }
    if (strcmp(argv[1], "scope") == 0) {
        void *plugin = dlopen("scope.so", RTLD_NOW | RTLD_LOCAL);
        void *dependency = dlopen("scope-dependency.so", RTLD_NOW | RTLD_NOLOAD);
        assert(plugin && dependency);
        void *private_symbol = original(dependency, "fixture_private_symbol");
        assert(private_symbol && !dlsym(RTLD_DEFAULT, "fixture_private_symbol"));
        void *(*look_up_default)(const char *) = symbol(plugin, "fixture_scope_default");
        void *(*look_up_next)(const char *) = symbol(plugin, "fixture_scope_next");
        assert(look_up_default("fixture_private_symbol") == private_symbol);
        assert(look_up_next("fixture_private_symbol") == private_symbol);
        in_shim(look_up_default("cuMemCreate"), "cuMemCreate");
        dlerror();
        assert(!look_up_default("not_a_symbol") && dlerror());
        dlclose(dependency);
        dlclose(plugin);
        return 0;
    }
    assert(initialize(0) == CUDA_SUCCESS);
    __typeof__(&cuDevicePrimaryCtxRetain) retain = symbol(driver, "cuDevicePrimaryCtxRetain");
    __typeof__(&cuDevicePrimaryCtxRelease) release = symbol(driver, "cuDevicePrimaryCtxRelease_v2");
    __typeof__(&cuCtxSetCurrent) set_current = symbol(driver, "cuCtxSetCurrent");
    fill = symbol(driver, "cuMemsetD8_v2");
    copy_to_host = symbol(driver, "cuMemcpyDtoH_v2");
    free_memory = symbol(driver, "cuMemFree_v2");
    CUcontext context;
    assert(retain(&context, 0) == CUDA_SUCCESS);
    assert(set_current(context) == CUDA_SUCCESS);
    if (strcmp(argv[1], "queries") == 0) {
        Query query = symbol(driver, "cuGetProcAddress");
        Query native = original(driver, "cuGetProcAddress");
        QueryV2 query2 = symbol(driver, "cuGetProcAddress_v2");
        QueryV2 native2 = original(driver, "cuGetProcAddress_v2");
        QueryV2 ptsz = symbol(RTLD_DEFAULT, "cuGetProcAddress_v2_ptsz");
        const struct { const char *name; int version; } cases[] = {
            {"cuCtxEnablePeerAccess", 13000}, {"cuCtxDisablePeerAccess", 13000},
            {"cuCtxDestroy", 2000}, {"cuCtxDestroy", 13000},
            {"cuDevicePrimaryCtxRelease", 7000}, {"cuDevicePrimaryCtxRelease", 13000},
            {"cuDevicePrimaryCtxReset", 7000}, {"cuDevicePrimaryCtxReset", 13000},
            {"cuMemAlloc", 13000}, {"cuIpcOpenMemHandle", 13000},
            {"cuMemCreate", 11000}, {"cuMemMap", 13000},
            {"cuMulticastBindMem", 12010}, {"cuMulticastBindMem", 13010},
            {"cuMulticastBindAddr", 12010}, {"cuMulticastBindAddr", 13010},
            {"cuGetProcAddress", 11030}, {"cuGetProcAddress", 12000},
        };
        for (unsigned i = 0; i < sizeof(cases) / sizeof(cases[0]); ++i) {
            void *expected = NULL, *actual = NULL;
            CUresult result = native(cases[i].name, &expected, cases[i].version, 0);
            assert(query(cases[i].name, &actual, cases[i].version, 0) == result);
            if (cases[i].version <= 13000)
                assert(result == CUDA_SUCCESS && expected);
            // Older drivers may reject the explicit-device multicast query. Match
            // its native result instead of requiring or skipping the newer API.
            if (result == CUDA_SUCCESS && expected) {
                Dl_info info;
                assert(dladdr(expected, &info) && info.dli_sname);
                in_shim(actual, info.dli_sname);
            } else {
                assert(actual == expected);
            }
            CUdriverProcAddressQueryResult expected_status = CU_GET_PROC_ADDRESS_SUCCESS;
            CUdriverProcAddressQueryResult actual_status = CU_GET_PROC_ADDRESS_SUCCESS;
            expected = actual = NULL;
            result = native2(cases[i].name, &expected, cases[i].version, 0, &expected_status);
            assert(query2(cases[i].name, &actual, cases[i].version, 0, &actual_status) == result);
            assert(actual_status == expected_status);
            if (cases[i].version <= 13000)
                assert(result == CUDA_SUCCESS && expected_status == CU_GET_PROC_ADDRESS_SUCCESS && expected);
            if (result == CUDA_SUCCESS && expected_status == CU_GET_PROC_ADDRESS_SUCCESS && expected) {
                Dl_info info;
                assert(dladdr(expected, &info) && info.dli_sname);
                in_shim(actual, info.dli_sname);
            } else {
                assert(actual == expected);
            }
        }
        void *address = NULL;
        CUdriverProcAddressQueryResult status;
        assert(query("cuMemAlloc", &address, 13000, 0) == CUDA_SUCCESS);
        allocate(address);
        assert(query("cuGetProcAddress", &address, 11030, 0) == CUDA_SUCCESS);
        assert(((Query)address)("cuMemAlloc", &address, 13000, 0) == CUDA_SUCCESS);
        allocate(address);
        assert(query("cuGetProcAddress", &address, 12000, 0) == CUDA_SUCCESS);
        assert(((QueryV2)address)("cuMemAlloc", &address, 13000, 0, &status) == CUDA_SUCCESS);
        assert(status == CU_GET_PROC_ADDRESS_SUCCESS);
        allocate(address);
        assert(ptsz("cuMemAlloc", &address, 13000, 0, &status) == CUDA_SUCCESS);
        assert(status == CU_GET_PROC_ADDRESS_SUCCESS);
        allocate(address);
        assert(query2("not_a_cuda_api", &address, 13000, 0, &status) == CUDA_SUCCESS);
        assert(!address && status == CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND);
        assert(query("cuDriverGetVersion", &address, 13000, 0) == CUDA_SUCCESS);
        assert(address == original(driver, "cuDriverGetVersion"));
    } else if (strcmp(argv[1], "runtime") == 0) {
        void *runtime = dlopen("libcudart.so.13", RTLD_NOW | RTLD_LOCAL);
        assert(runtime);
        const char *names[] = {"cudaGetDriverEntryPoint", "cudaGetDriverEntryPoint_ptsz",
            "cudaGetDriverEntryPointByVersion", "cudaGetDriverEntryPointByVersion_ptsz"};
        for (unsigned i = 0; i < 4; ++i) {
            void *resolver = symbol(runtime, names[i]);
            in_shim(resolver, names[i]);
            void *address = NULL;
            int status = -1;
            int result = i < 2 ? ((RuntimeQuery)resolver)("cuMemAlloc", &address, 0, &status)
                              : ((RuntimeVersionQuery)resolver)("cuMemAlloc", &address, 13000, 0, &status);
            assert(result == 0 && status == 0);
            in_shim(address, "cuMemAlloc_v2");
            allocate(address);
            const char *peers[] = {"cuCtxEnablePeerAccess", "cuCtxDisablePeerAccess"};
            for (unsigned j = 0; j < 2; ++j) {
                result = i < 2 ? ((RuntimeQuery)resolver)(peers[j], &address, 0, &status)
                               : ((RuntimeVersionQuery)resolver)(peers[j], &address, 13000, 0, &status);
                assert(result == 0 && status == 0);
                in_shim(address, peers[j]);
            }
        }
        dlclose(runtime);
    } else if (strcmp(argv[1], "local-lifetime") == 0) {
        void *alloc = symbol(driver, "cuMemAlloc_v2");
        assert(dlclose(driver) == 0);
        driver = dlopen("libcuda.so.1", RTLD_NOW | RTLD_NOLOAD);
        assert(driver);
        assert(dlclose(driver) == 0);
        allocate(alloc);
        driver = NULL;
    } else {
        fprintf(stderr, "unknown case: %s\n", argv[1]);
        return 1;
    }
    assert(set_current(NULL) == CUDA_SUCCESS);
    assert(release(0) == CUDA_SUCCESS);
    if (driver)
        dlclose(driver);
    return 0;
}
