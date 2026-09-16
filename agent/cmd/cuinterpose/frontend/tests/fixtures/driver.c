// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include "cuda.h"
#include <string.h>
#include <errno.h>
#include <unistd.h>
#include <assert.h>
#include <dlfcn.h>
#include <sys/wait.h>
#include <stdlib.h>

static struct fixture_call last;
int cuInit(unsigned flags) {
    return flags ? 1 : 0;
}

// Models a runtime-only allocation path that initializes the driver but never
// calls VMM. Lookup through the default scope permits the cuInit interceptor.
int fixture_private_runtime(void) {
    int (*initialize)(unsigned) = dlsym(RTLD_DEFAULT, "cuInit");
    return initialize ? initialize(0) : 3;
}

static int anonymous_entry(void) { return 78; }

const struct fixture_call *fixture_last_call(void) {
    return &last;
}

int cuMemCreate(uint64_t *output, size_t size, const void *properties, uint64_t flags) {
    last.size = size;
    last.properties = properties;
    last.flags = flags;
    if (!output)
        return 1;
    if (flags == 999)
        return 2; // Driver failure must leave application output untouched.
    *output = 0xabcdef;
    return 0;
}

int cuMemMap(uint64_t address, size_t size, size_t offset, uint64_t handle, uint64_t flags) {
    last.member = address;
    last.size = size;
    last.offset = offset;
    last.handle = handle;
    last.flags = flags;
    return 101;
}

int cuMulticastBindMem(uint64_t handle, size_t offset, uint64_t member,
                       size_t member_offset, size_t size, uint64_t flags) {
    last.handle = handle;
    last.offset = offset;
    last.member = member;
    last.member_offset = member_offset;
    last.size = size;
    last.flags = flags;
    last.device = -1;
    return 201;
}

int cuMulticastBindMem_v2(uint64_t handle, int device, size_t offset, uint64_t member,
                          size_t member_offset, size_t size, uint64_t flags) {
    last.handle = handle;
    last.offset = offset;
    last.member = member;
    last.member_offset = member_offset;
    last.size = size;
    last.flags = flags;
    last.device = device;
    return 202;
}

int cuMulticastBindAddr(uint64_t handle, size_t offset, uint64_t address,
                        size_t size, uint64_t flags) {
    last.handle = handle;
    last.offset = offset;
    last.member = address;
    last.size = size;
    last.flags = flags;
    last.device = -1;
    return 301;
}

int cuMulticastBindAddr_v2(uint64_t handle, int device, size_t offset,
                           uint64_t address, size_t size, uint64_t flags) {
    last.handle = handle;
    last.offset = offset;
    last.member = address;
    last.size = size;
    last.flags = flags;
    last.device = device;
    return 302;
}

int cuFixtureUnwrapped(void) {
    return 77;
}

int cuFixtureQuery(const char *name, void **output, int version, uint64_t flags, int *status) {
    last.version = version;
    last.query_flags = flags;
    if (!name || !output)
        return 1;
    if (strcmp(name, "query-error") == 0)
        return 1;
    if (status)
        *status = 0;
    const char *identity = getenv("CUINTERPOSE_TEST_IDENTITY");
    if (identity) {
        if (strcmp(identity, "anonymous") == 0)
            *output = (void *)anonymous_entry;
        else if (strcmp(identity, "mismatch") == 0)
            *output = strcmp(name, "cuMemMap") == 0 ? (void *)cuInit : (void *)cuMemMap;
        else if (strcmp(identity, "alias") == 0)
            *output = (void *)cuFixtureUnwrapped;
        else if (strcmp(identity, "foreign") == 0) {
            void *plugin = dlopen("plugin.so", RTLD_NOW | RTLD_LOCAL);
            assert(plugin);
            *output = dlsym(plugin, "cuMemCreate");
        } else assert(0);
        return 0;
    }
    if (strcmp(name, "query-status-failure") == 0) {
        *output = (void *)cuMemMap;
        if (status)
            *status = 2;
        return 0;
    }
    // -Bsymbolic-functions is essential: pointers returned by this provider
    // must point to its real CUDA-named definitions, not preempted wrappers.
    if (strcmp(name, "cuInit") == 0)
        *output = (void *)cuInit;
    else if (strcmp(name, "cuMemCreate") == 0)
        *output = (void *)cuMemCreate;
    else if (strcmp(name, "cuMemMap") == 0)
        *output = (void *)cuMemMap;
    else if (strcmp(name, "cuMulticastBindMem") == 0)
        *output = version >= 13010 ? (void *)cuMulticastBindMem_v2 : (void *)cuMulticastBindMem;
    else if (strcmp(name, "cuMulticastBindAddr") == 0)
        *output = version >= 13010 ? (void *)cuMulticastBindAddr_v2 : (void *)cuMulticastBindAddr;
    else if (strcmp(name, "cuGetProcAddress") == 0)
        *output = version >= 12000 ? (void *)cuGetProcAddress_v2 : (void *)cuGetProcAddress;
    else if (strcmp(name, "cuGetProcAddress_v2") == 0)
        *output = (void *)cuGetProcAddress_v2;
    else if (strcmp(name, "cuFixtureUnwrapped") == 0)
        *output = (void *)cuFixtureUnwrapped;
    else {
        *output = NULL;
        if (status)
            *status = 1;
    }
    return 0;
}

int cuGetProcAddress(const char *name, void **output, int version, uint64_t flags) {
    return cuFixtureQuery(name, output, version, flags, NULL);
}

int cuGetProcAddress_v2(const char *name, void **output, int version, uint64_t flags, int *status) {
    return cuFixtureQuery(name, output, version, flags, status);
}
