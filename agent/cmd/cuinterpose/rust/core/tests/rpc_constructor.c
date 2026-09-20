// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// dlopen executes this constructor while the caller owns the loader lock.
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>

struct properties {
    int type;
    unsigned handles;
    struct { int type, id; } location;
    void *win32;
    unsigned char flags[8];
};
extern void rpc_fail_startup(int);
extern int cuInit(unsigned);
extern int cuMemCreate(uint64_t *, size_t, const struct properties *, uint64_t);

uint64_t rpc_constructor_handle = UINT64_C(0xAAAA);
int rpc_constructor_result = -1;

__attribute__((constructor)) static void initialize(void) {
    const struct properties properties = { .type = 1, .handles = 1,
                                          .location = { .type = 1, .id = 0 } };
    const char *failure = getenv("CUINTERPOSE_TEST_STARTUP_FAILURE");
    rpc_fail_startup(failure ? atoi(failure) : 0);
    fputs("RPC constructor entered\n", stderr);
    rpc_constructor_result = cuInit(0);
    if (rpc_constructor_result == 0)
        rpc_constructor_result = cuMemCreate(&rpc_constructor_handle, 4096, &properties, 0);
    fputs("RPC constructor returned\n", stderr);
}
