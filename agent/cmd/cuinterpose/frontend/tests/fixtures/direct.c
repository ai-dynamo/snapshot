// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include "cuda.h"
#include <assert.h>
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>

int main(void) {
    assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") == NULL);
    Dl_info info;
    assert(dladdr((void *)cuMemCreate, &info) != 0);
    assert(strstr(info.dli_fname, "libcuinterpose.so") != NULL);
    unsigned char prop[32] = {0};
    size_t granularity = 0;
    assert(cuMemGetAllocationGranularity(&granularity, prop, 0) == 0 && granularity == 4096);
    assert(cuMulticastGetGranularity(&granularity, prop, 0) == 0 && granularity == 65536);
    assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") == NULL);
    uint64_t handle = 44;
    assert(cuMemCreate(&handle, 4096, prop, 7) == 0);
    assert(handle == 0xabcdef);
    assert(getenv("CUINTERPOSE_TEST_CORE_INITIALIZED") != NULL);
    const struct fixture_call *call = fixture_last_call();
    assert(call->size == 4096 && call->flags == 7 && call->properties == prop);
    assert(cuMemMap(0x1234, 8192, 128, handle, 9) == 101);
    assert(call->member == 0x1234 && call->size == 8192 && call->offset == 128);
    assert(call->handle == handle && call->flags == 9);
    handle = 44;
    assert(cuMemCreate(&handle, 4096, prop, 999) == 2);
    assert(handle == 44);
    return 0;
}
