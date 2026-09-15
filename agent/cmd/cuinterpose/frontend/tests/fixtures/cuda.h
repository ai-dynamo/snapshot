// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Deliberately independent of the production bindings and CUDA toolkit. These
// fixtures test the Linux/amd64 entry-point ABI, not CUDA device behavior.
#ifndef CUINTERPOSE_LOADER_TEST_CUDA_H
#define CUINTERPOSE_LOADER_TEST_CUDA_H

#include <stddef.h>
#include <stdint.h>

typedef int (*query_v1)(const char *, void **, int, uint64_t);
typedef int (*query_v2)(const char *, void **, int, uint64_t, int *);
typedef int (*runtime_query)(const char *, void **, uint64_t, int *);
typedef int (*runtime_version_query)(const char *, void **, unsigned, uint64_t, int *);
typedef int (*create_fn)(uint64_t *, size_t, const void *, uint64_t);
typedef int (*bind_v1)(uint64_t, size_t, uint64_t, size_t, size_t, uint64_t);
typedef int (*bind_v2)(uint64_t, int, size_t, uint64_t, size_t, size_t, uint64_t);
typedef int (*bind_addr_v1)(uint64_t, size_t, uint64_t, size_t, uint64_t);
typedef int (*bind_addr_v2)(uint64_t, int, size_t, uint64_t, size_t, uint64_t);

struct fixture_call {
    uint64_t handle, member, flags;
    size_t offset, member_offset, size;
    int device;
    const void *properties;
    int version;
    uint64_t query_flags;
};

int cuMemCreate(uint64_t *, size_t, const void *, uint64_t);
int cuInit(unsigned);
int cuMemMap(uint64_t, size_t, size_t, uint64_t, uint64_t);
int cuMulticastBindMem(uint64_t, size_t, uint64_t, size_t, size_t, uint64_t);
int cuMulticastBindMem_v2(uint64_t, int, size_t, uint64_t, size_t, size_t, uint64_t);
int cuMulticastBindAddr(uint64_t, size_t, uint64_t, size_t, uint64_t);
int cuMulticastBindAddr_v2(uint64_t, int, size_t, uint64_t, size_t, uint64_t);
int cuGetProcAddress(const char *, void **, int, uint64_t);
int cuGetProcAddress_v2(const char *, void **, int, uint64_t, int *);
int cuFixtureQuery(const char *, void **, int, uint64_t, int *);
int cuFixtureUnwrapped(void);
const struct fixture_call *fixture_last_call(void);

#endif
