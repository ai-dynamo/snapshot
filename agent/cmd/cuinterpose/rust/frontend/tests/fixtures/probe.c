// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include "cuda.h"
#include <assert.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void *symbol(void *handle, const char *name) {
    void *pointer = dlsym(handle, name);
    if (!pointer)
        fprintf(stderr, "missing symbol %s: %s\n", name, dlerror());
    assert(pointer);
    return pointer;
}

static void in_shim(void *pointer, const char *name) {
    Dl_info info;
    assert(dladdr(pointer, &info) != 0);
    assert(strstr(info.dli_fname, "libcuinterpose.so"));
    assert(info.dli_sname && strcmp(info.dli_sname, name) == 0);
    assert(info.dli_saddr == pointer);
}

int main(int argc, char **argv) {
    assert(argc == 2);
    assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") == NULL);
    if (strcmp(argv[1], "next") == 0) {
        int (*call)(void) = symbol(RTLD_DEFAULT, "fixture_lookup_next");
        assert(call() == 93);
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") == NULL);
        return 0;
    }

    // No link-time CUDA dependency: this really is RTLD_LOCAL discovery.
    void *driver = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
    assert(driver);
    void *runtime = dlopen("libcudart.so.13", RTLD_NOW | RTLD_LOCAL);
    assert(runtime);
    void *(*original)(void *, const char *) = dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34");
    assert(original);
    const struct fixture_call *(*last_call)(void) = symbol(driver, "fixture_last_call");
    const struct fixture_call *call = last_call();
    query_v1 query = symbol(driver, "cuGetProcAddress");
    query_v2 query2 = symbol(driver, "cuGetProcAddress_v2");
    in_shim((void *)query, "cuGetProcAddress");
    in_shim((void *)query2, "cuGetProcAddress_v2");
    assert(symbol(RTLD_DEFAULT, "cuMemCreate") == symbol(driver, "cuMemCreate"));
    assert(symbol(driver, "cuMemCreate") != original(driver, "cuMemCreate"));
    assert(symbol(driver, "cuFixtureUnwrapped") == original(driver, "cuFixtureUnwrapped"));

    if (strcmp(argv[1], "lookup") == 0) {
        dlerror();
        assert(dlsym(driver, "cuMemRelease") == NULL); // Shim exports it; provider does not.
        assert(dlerror() != NULL);
        dlerror();
        assert(dlsym(driver, "not_a_cuda_api") == NULL);
        assert(dlerror() != NULL);
        void *libc = dlopen("libc.so.6", RTLD_NOW);
        assert(libc);
        assert(dlsym(libc, "cuMemCreate") == NULL);
        assert(symbol(libc, "malloc") == original(libc, "malloc"));
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") == NULL);
        dlclose(libc);
    } else if (strcmp(argv[1], "queries") == 0) {
        void *pointer = NULL;
        int status = -1;
        assert(query("cuMemCreate", &pointer, 11000, 5) == 0);
        in_shim(pointer, "cuMemCreate");
        assert(query2("cuMemMap", &pointer, 13010, 8, &status) == 0);
        assert(status == 0 && call->version == 13010 && call->query_flags == 8);
        in_shim(pointer, "cuMemMap");
        // Self lookup must return a callable ABI, not just the expected name.
        assert(query("cuGetProcAddress", &pointer, 11000, 0) == 0);
        in_shim(pointer, "cuGetProcAddress");
        assert(((query_v1)pointer)("cuMemCreate", &pointer, 11000, 0) == 0);
        in_shim(pointer, "cuMemCreate");
        assert(query("cuGetProcAddress", &pointer, 12000, 0) == 0);
        in_shim(pointer, "cuGetProcAddress_v2");
        assert(((query_v2)pointer)("cuMemMap", &pointer, 12000, 0, &status) == 0);
        in_shim(pointer, "cuMemMap");
        query_v2 ptsz = symbol(RTLD_DEFAULT, "cuGetProcAddress_v2_ptsz");
        assert(ptsz("cuMemMap", &pointer, 13010, 0, &status) == 0);
        assert(call->query_flags == 2);
        assert(ptsz("cuMemMap", &pointer, 13010, 1, &status) == 0);
        assert(call->query_flags == 1);
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") == NULL);
    } else if (strcmp(argv[1], "missing") == 0) {
        void *pointer = (void *)0x1234;
        int status = -1;
        assert(query2("query-error", &pointer, 13010, 0, &status) == 1);
        assert(pointer == (void *)0x1234 && status == -1);
        assert(query2("no-such-api", &pointer, 13010, 0, &status) == 0);
        assert(pointer == NULL && status == 1);
        assert(query2("query-status-failure", &pointer, 13010, 0, &status) == 0);
        assert(status == 2 && pointer == original(driver, "cuMemMap"));
        assert(query("cuFixtureUnwrapped", &pointer, 13010, 0) == 0);
        assert(pointer == original(driver, "cuFixtureUnwrapped"));
    } else if (strcmp(argv[1], "bindings") == 0) {
        void *pointer;
        assert(query("cuMulticastBindMem", &pointer, 12000, 0) == 0);
        in_shim(pointer, "cuMulticastBindMem");
        assert(((bind_v1)pointer)(11, 22, 33, 44, 55, 66) == 201);
        assert(call->handle == 11 && call->offset == 22 && call->member == 33);
        assert(call->member_offset == 44 && call->size == 55 && call->flags == 66 && call->device == -1);
        assert(query("cuMulticastBindMem", &pointer, 13010, 0) == 0);
        in_shim(pointer, "cuMulticastBindMem_v2");
        assert(((bind_v2)pointer)(11, 7, 22, 33, 44, 55, 66) == 202);
        assert(call->handle == 11 && call->device == 7 && call->offset == 22);
        assert(call->member == 33 && call->member_offset == 44 && call->size == 55 && call->flags == 66);
        assert(query("cuMulticastBindAddr", &pointer, 12000, 0) == 0);
        in_shim(pointer, "cuMulticastBindAddr");
        assert(((bind_addr_v1)pointer)(11, 22, 33, 44, 55) == 301);
        assert(call->member == 33 && call->size == 44 && call->flags == 55 && call->device == -1);
        assert(query("cuMulticastBindAddr", &pointer, 13010, 0) == 0);
        in_shim(pointer, "cuMulticastBindAddr_v2");
        assert(((bind_addr_v2)pointer)(11, 8, 22, 33, 44, 55) == 302);
        assert(call->handle == 11 && call->device == 8 && call->offset == 22);
        assert(call->member == 33 && call->size == 44 && call->flags == 55);
    } else if (strcmp(argv[1], "runtime") == 0) {
        const char *names[] = {"cudaGetDriverEntryPoint", "cudaGetDriverEntryPoint_ptsz",
            "cudaGetDriverEntryPointByVersion", "cudaGetDriverEntryPointByVersion_ptsz"};
        for (unsigned i = 0; i != 4; ++i) {
            void *resolver = symbol(runtime, names[i]);
            in_shim(resolver, names[i]);
            void *pointer = NULL;
            int status = -1;
            int result = i < 2 ? ((runtime_query)resolver)("cuMulticastBindMem", &pointer, 0, &status)
                              : ((runtime_version_query)resolver)("cuMulticastBindMem", &pointer, 12000, 0, &status);
            assert(result == 0 && status == 0);
            in_shim(pointer, i < 2 ? "cuMulticastBindMem_v2" : "cuMulticastBindMem");
            assert(call->query_flags == (i % 2 ? 2 : 0));
            pointer = (void *)0x1234;
            result = i < 2 ? ((runtime_query)resolver)("query-error", &pointer, 0, &status)
                           : ((runtime_version_query)resolver)("query-error", &pointer, 12000, 0, &status);
            assert(result == 1 && pointer == (void *)0x1234);
        }
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") == NULL);
    } else if (strcmp(argv[1], "local-lifetime") == 0) {
        create_fn create = symbol(driver, "cuMemCreate");
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") == NULL);
        assert(dlclose(runtime) == 0);
        assert(dlclose(driver) == 0);
        void *retained = dlopen("libcuda.so.1", RTLD_NOW | RTLD_NOLOAD);
        assert(retained); // The front end owns a provider reference now.
        dlclose(retained);
        uint64_t handle = 0;
        assert(create(&handle, 4096, NULL, 0) == 0 && handle == 0xabcdef);
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") != NULL);
        return 0;
    } else if (strcmp(argv[1], "missing-core") == 0 || strcmp(argv[1], "bad-core") == 0 ||
               strcmp(argv[1], "short-version-core") == 0 || strcmp(argv[1], "short-size-core") == 0) {
        create_fn create = symbol(driver, "cuMemCreate");
        uint64_t handle = 42;
        assert(create(&handle, 4096, NULL, 0) == 3);
        assert(handle == 42);
    } else {
        fprintf(stderr, "unknown case: %s\n", argv[1]);
        return 1;
    }
    dlclose(runtime);
    dlclose(driver);
    return 0;
}
