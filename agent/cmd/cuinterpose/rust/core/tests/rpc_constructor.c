// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// dlopen executes this constructor while the caller owns the loader lock.
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

struct properties {
    int type;
    unsigned handles;
    struct { int type, id; } location;
    void *win32;
    unsigned char flags[8];
};
struct stats {
    uint64_t allocations, handles, mappings, multicasts, exports, raw, unsupported;
    unsigned phase;
};
extern void rpc_fail_startup(int);
extern int cuMemCreate(uint64_t *, size_t, const struct properties *, uint64_t);
extern void cuinterpose_debug_stats(struct stats *);

uint64_t rpc_constructor_handle = UINT64_C(0xAAAA);
struct stats rpc_constructor_stats;
int rpc_constructor_result = -1;

__attribute__((constructor)) static void initialize(void) {
    const struct properties properties = { .type = 1, .handles = 1,
                                          .location = { .type = 1, .id = 0 } };
    const char *failure = getenv("CUINTERPOSE_TEST_STARTUP_FAILURE");
    rpc_fail_startup(failure ? atoi(failure) : 0);
    memset(&rpc_constructor_stats, 0xA5, sizeof(rpc_constructor_stats));
    fputs("RPC constructor entered\n", stderr);
    rpc_constructor_result = cuMemCreate(&rpc_constructor_handle, 4096, &properties, 0);
    cuinterpose_debug_stats(&rpc_constructor_stats);
    fputs("RPC constructor returned\n", stderr);
}
