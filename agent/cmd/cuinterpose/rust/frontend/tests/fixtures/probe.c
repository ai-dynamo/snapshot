// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include "cuda.h"
#include <assert.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <link.h>

create_fn fixture_create;

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
    int early_plugin = strcmp(argv[1], "early-plugin") == 0 || strcmp(argv[1], "early-plugin-nested") == 0;
    if (!early_plugin && strcmp(argv[1], "chain-before") != 0)
        assert(symbol(RTLD_DEFAULT, "cuMemCreate") == symbol(driver, "cuMemCreate"));
    assert(symbol(driver, "cuMemCreate") != original(driver, "cuMemCreate"));
    assert(symbol(driver, "cuFixtureUnwrapped") == original(driver, "cuFixtureUnwrapped"));

    if (early_plugin) {
        void *plugin = dlopen("plugin.so", RTLD_NOW | RTLD_NOLOAD);
        assert(plugin);
        void *foreign = original(plugin, "cuMemCreate");
        assert(foreign);
        // A non-CUDA explicit/default lookup must still reach the selected
        // plugin. CUDA-qualified lookup must return the actual frontend, even
        // though the plugin precedes it in the global search order.
        assert(symbol(plugin, "cuMemCreate") == foreign);
        assert(symbol(RTLD_DEFAULT, "cuMemCreate") == foreign);
        create_fn create = symbol(driver, "cuMemCreate");
        in_shim((void *)create, "cuMemCreate");
        uint64_t allocation = 0;
        assert(create(&allocation, 4096, NULL, 17) == 0 && allocation == 0xabcdef);
        const char *names[] = {"cudaGetDriverEntryPoint", "cudaGetDriverEntryPoint_ptsz",
            "cudaGetDriverEntryPointByVersion", "cudaGetDriverEntryPointByVersion_ptsz"};
        for (unsigned bad = 0; bad < 2; ++bad) {
            if (bad)
                assert(setenv("CUINTERPOSE_TEST_IDENTITY", "foreign", 1) == 0);
            void *pointer = (void *)0x1234;
            int status = -1;
            int expected = bad ? 801 : 0;
            assert(query("cuMemCreate", &pointer, 13010, 0) == expected);
            if (bad) assert(pointer == NULL);
            else {
                in_shim(pointer, "cuMemCreate");
                assert(((create_fn)pointer)(&allocation, 4096, NULL, 17) == 0 && allocation == 0xabcdef);
            }
            assert(query2("cuMemCreate", &pointer, 13010, 0, &status) == expected && status == 0);
            if (bad) assert(pointer == NULL);
            else {
                in_shim(pointer, "cuMemCreate");
                assert(((create_fn)pointer)(&allocation, 4096, NULL, 17) == 0 && allocation == 0xabcdef);
            }
            for (unsigned i = 0; i < 4; ++i) {
                void *resolver = symbol(runtime, names[i]);
                int result = i < 2 ? ((runtime_query)resolver)("cuMemCreate", &pointer, 0, &status)
                    : ((runtime_version_query)resolver)("cuMemCreate", &pointer, 13010, 0, &status);
                assert(result == expected && status == 0);
                if (bad) assert(pointer == NULL);
                else {
                    in_shim(pointer, "cuMemCreate");
                    assert(((create_fn)pointer)(&allocation, 4096, NULL, 17) == 0 && allocation == 0xabcdef);
                }
            }
        }
        unsetenv("CUINTERPOSE_TEST_IDENTITY");
        dlclose(plugin);
    } else if (strcmp(argv[1], "chain-before") == 0 || strcmp(argv[1], "chain-after") == 0) {
        create_fn create = symbol(RTLD_DEFAULT, "cuMemCreate");
        bind_v1 bind = symbol(RTLD_DEFAULT, "cuMulticastBindMem");
        in_shim(symbol(driver, "cuMemCreate"), "cuMemCreate");
        if (strcmp(argv[1], "chain-before") == 0) {
            Dl_info info;
            assert(dladdr((void *)create, &info) != 0 && strstr(info.dli_fname, "chain.so"));
        } else {
            in_shim((void *)create, "cuMemCreate");
        }
        uint64_t handle = 0;
        assert(create(&handle, 4096, NULL, 0) == 0 && handle == 0xabcdef);
        assert(bind(11, 22, 33, 44, 55, 66) == 201);
        const unsigned *(*counts)(void) = symbol(RTLD_DEFAULT, "fixture_chain_counts");
        assert(counts()[0] == 1 && counts()[1] == 1);
        counts = symbol(driver, "fixture_driver_counts");
        assert(counts()[0] == 1 && counts()[1] == 1);
        void *core = dlopen("libcuinterpose_core.so", RTLD_NOW | RTLD_NOLOAD);
        assert(core);
        counts = symbol(core, "fixture_core_counts");
        assert(counts()[0] == 1 && counts()[1] == 1);
        dlclose(core);
    } else if (strcmp(argv[1], "providers") == 0) {
        const char *paths[] = {"plugin.so", "libcuda.so.fake"};
        for (unsigned i = 0; i < 2; ++i) {
            void *plugin = dlopen(paths[i], RTLD_NOW | RTLD_LOCAL);
            assert(plugin);
            create_fn create = symbol(plugin, "cuMemCreate");
            assert((void *)create == original(plugin, "cuMemCreate"));
            uint64_t handle = 0;
            assert(create(&handle, 4096, NULL, 0) == 88 && handle == 88);
            dlclose(plugin);
        }
        // A non-CUDA handle's dependency exports CUDA names; explicit lookup
        // must preserve that handle's result rather than inject our wrapper.
        assert(symbol(runtime, "cuMemCreate") == original(runtime, "cuMemCreate"));
        void *isolated = dlmopen(LM_ID_NEWLM, "libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
        assert(isolated);
        assert(symbol(isolated, "cuMemCreate") == original(isolated, "cuMemCreate"));
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") == NULL);
        dlclose(isolated);
    } else if (strcmp(argv[1], "identities") == 0 || strcmp(argv[1], "identities-nested") == 0) {
        const char *identities[] = {"anonymous", "alias", "mismatch", "foreign"};
        const char *runtime_names[] = {"cudaGetDriverEntryPoint", "cudaGetDriverEntryPoint_ptsz",
            "cudaGetDriverEntryPointByVersion", "cudaGetDriverEntryPointByVersion_ptsz"};
        const char *tracked[] = {"cuInit", "cuMemCreate", "cuMemRelease",
            "cuMemRetainAllocationHandle", "cuMemMap", "cuMemUnmap", "cuMemSetAccess",
            "cuMemExportToShareableHandle", "cuMemImportFromShareableHandle",
            "cuMemGetAllocationPropertiesFromHandle", "cuMulticastCreate",
            "cuMulticastAddDevice", "cuMulticastBindMem", "cuMulticastBindMem_v2",
            "cuMulticastBindAddr", "cuMulticastBindAddr_v2", "cuMulticastGetGranularity",
            "cuMulticastUnbind", "cuGetProcAddress", "cuGetProcAddress_v2"};
        for (unsigned i = 0; i < 4; ++i) {
            assert(setenv("CUINTERPOSE_TEST_IDENTITY", identities[i], 1) == 0);
            void *pointer = (void *)0x1234;
            int status = -1;
            for (unsigned k = 0; k < sizeof(tracked) / sizeof(tracked[0]); ++k) {
                assert(query(tracked[k], &pointer, 13010, 0) == 801 && pointer == NULL);
                assert(query2(tracked[k], &pointer, 13010, 0, &status) == 801 && pointer == NULL);
                assert(status == 0); // Real query status remains untouched.
                query_v2 ptsz = symbol(RTLD_DEFAULT, "cuGetProcAddress_v2_ptsz");
                assert(ptsz(tracked[k], &pointer, 13010, 0, &status) == 801 && pointer == NULL);
                for (unsigned j = 0; j < 4; ++j) {
                    void *resolver = symbol(runtime, runtime_names[j]);
                    int result = j < 2 ? ((runtime_query)resolver)(tracked[k], &pointer, 0, &status)
                        : ((runtime_version_query)resolver)(tracked[k], &pointer, 13010, 0, &status);
                    assert(result == 801 && pointer == NULL && status == 0);
                }
            }
            assert(query("cuFixtureUnwrapped", &pointer, 13010, 0) == 0 && pointer);
        }
        unsetenv("CUINTERPOSE_TEST_IDENTITY");
        void *pointer;
        assert(query("cuMemCreate", &pointer, 13010, 0) == 0); // Refusal is not poison.
        in_shim(pointer, "cuMemCreate");
    } else if (strcmp(argv[1], "ready-failure") == 0) {
        assert(setenv("CUINTERPOSE_TEST_READY_FAILURE", "1", 1) == 0);
        void *pointer = (void *)0x1234;
        int (*initialize)(unsigned) = symbol(driver, "cuInit");
        assert(initialize(0) == 3);
        assert(query("cuMemCreate", &pointer, 13010, 0) == 3 && pointer == NULL);
        unsetenv("CUINTERPOSE_TEST_READY_FAILURE");
        assert(initialize(0) == 0);
    } else if (strcmp(argv[1], "constructor-concurrent") == 0) {
        fixture_create = symbol(driver, "cuMemCreate");
        void *plugin = dlopen("constructor.so", RTLD_NOW | RTLD_LOCAL);
        assert(plugin);
        void (*join_worker)(void) = symbol(plugin, "fixture_join_worker");
        join_worker();
        uint64_t handle = 0;
        assert(fixture_create(&handle, 4096, NULL, 0) == 0 && handle == 0xabcdef);
        dlclose(plugin);
    } else if (strcmp(argv[1], "constructor-reentry") == 0) {
        create_fn create = symbol(driver, "cuMemCreate");
        uint64_t handle = 0;
        assert(create(&handle, 4096, NULL, 0) == 0);
        assert(handle == 0xabcdef);
    } else if (strcmp(argv[1], "lookup") == 0) {
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
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") != NULL);
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
    } else if (strcmp(argv[1], "runtime-nested") == 0) {
        const char *names[] = {"cudaGetDriverEntryPoint", "cudaGetDriverEntryPoint_ptsz",
            "cudaGetDriverEntryPointByVersion", "cudaGetDriverEntryPointByVersion_ptsz"};
        for (unsigned i = 0; i != 4; ++i) {
            void *resolver = symbol(runtime, names[i]);
            for (unsigned version = 12000; version <= 13010; version += 1010) {
                int actual_version = i < 2 ? 13010 : (int)version;
                int status = -1;
                void *pointer = NULL;
                int result = i < 2 ? ((runtime_query)resolver)("cuMemCreate", &pointer, 0, &status)
                    : ((runtime_version_query)resolver)("cuMemCreate", &pointer, version, 0, &status);
                assert(result == 0 && status == 0);
                in_shim(pointer, "cuMemCreate");
                uint64_t allocation = 0;
                assert(((create_fn)pointer)(&allocation, 4096, NULL, 17) == 0);
                assert(allocation == 0xabcdef && call->size == 4096 && call->flags == 17);

                result = i < 2 ? ((runtime_query)resolver)("cuMulticastBindMem", &pointer, 0, &status)
                    : ((runtime_version_query)resolver)("cuMulticastBindMem", &pointer, version, 0, &status);
                assert(result == 0 && status == 0);
                assert(call->version == actual_version && call->query_flags == (i % 2 ? 2 : 0));
                if (actual_version >= 13010) {
                    in_shim(pointer, "cuMulticastBindMem_v2");
                    assert(((bind_v2)pointer)(11, 7, 22, 33, 44, 55, 66) == 202);
                    assert(call->device == 7);
                } else {
                    in_shim(pointer, "cuMulticastBindMem");
                    assert(((bind_v1)pointer)(11, 22, 33, 44, 55, 66) == 201);
                    assert(call->device == -1);
                }
                assert(call->handle == 11 && call->offset == 22 && call->member == 33);
                assert(call->member_offset == 44 && call->size == 55 && call->flags == 66);

                result = i < 2 ? ((runtime_query)resolver)("cuMulticastBindAddr", &pointer, 0, &status)
                    : ((runtime_version_query)resolver)("cuMulticastBindAddr", &pointer, version, 0, &status);
                assert(result == 0 && status == 0);
                if (actual_version >= 13010) {
                    in_shim(pointer, "cuMulticastBindAddr_v2");
                    assert(((bind_addr_v2)pointer)(11, 8, 22, 33, 44, 55) == 302);
                    assert(call->device == 8);
                } else {
                    in_shim(pointer, "cuMulticastBindAddr");
                    assert(((bind_addr_v1)pointer)(11, 22, 33, 44, 55) == 301);
                    assert(call->device == -1);
                }
                assert(call->handle == 11 && call->offset == 22 && call->member == 33);
                assert(call->size == 44 && call->flags == 55);

                result = i < 2 ? ((runtime_query)resolver)("cuGetProcAddress", &pointer, 0, &status)
                    : ((runtime_version_query)resolver)("cuGetProcAddress", &pointer, version, 0, &status);
                assert(result == 0 && status == 0);
                in_shim(pointer, "cuGetProcAddress_v2");
                assert(((query_v2)pointer)("cuMemCreate", &pointer, 13010, 0, &status) == 0);
                in_shim(pointer, "cuMemCreate");
            }
            const char *failures[] = {"query-error", "query-status-failure", "no-such-api"};
            for (unsigned j = 0; j < 3; ++j) {
                void *pointer = (void *)0x1234;
                int status = -1;
                int result = i < 2 ? ((runtime_query)resolver)(failures[j], &pointer, 0, &status)
                    : ((runtime_version_query)resolver)(failures[j], &pointer, 13010, 0, &status);
                if (j == 0)
                    assert(result == 1 && pointer == (void *)0x1234 && status == -1);
                else if (j == 1)
                    assert(result == 0 && status == 2 && pointer == original(driver, "cuMemMap"));
                else
                    assert(result == 0 && status == 1 && pointer == NULL);
            }
            // The outer resolver must still check the family of an exact
            // shim-owned pointer. A name/provider match alone is insufficient.
            assert(setenv("CUINTERPOSE_TEST_RUNTIME_WRAPPER", "cuMemMap", 1) == 0);
            void *pointer = (void *)0x1234;
            int status = -1;
            int result = i < 2 ? ((runtime_query)resolver)("cuMemCreate", &pointer, 0, &status)
                : ((runtime_version_query)resolver)("cuMemCreate", &pointer, 13010, 0, &status);
            assert(result == 801 && pointer == NULL && status == 0);
            unsetenv("CUINTERPOSE_TEST_RUNTIME_WRAPPER");
        }
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") != NULL);
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
        assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") != NULL);
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
