// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include "cuda.h"
#include <assert.h>
#include <dlfcn.h>

static unsigned counts[2];
const unsigned *fixture_chain_counts(void) { return counts; }

int cuMemCreate(uint64_t *out, size_t size, const void *prop, uint64_t flags) {
    assert(++counts[0] == 1);
    create_fn next = dlsym(RTLD_NEXT, "cuMemCreate");
    assert(next);
    return next(out, size, prop, flags);
}

int cuMulticastBindMem(uint64_t handle, size_t offset, uint64_t member,
                       size_t member_offset, size_t size, uint64_t flags) {
    assert(++counts[1] == 1);
    bind_v1 next = dlsym(RTLD_NEXT, "cuMulticastBindMem");
    assert(next);
    return next(handle, offset, member, member_offset, size, flags);
}
