// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "cuda.h"
#include <assert.h>
#include <dlfcn.h>
#include <stdlib.h>

static int runtime_lookup(const char *name, void **output, int version,
                          uint64_t flags, int *status) {
    const char *wrapper = getenv("CUINTERPOSE_TEST_RUNTIME_WRAPPER");
    if (wrapper) {
        // Model an already-interposed result independently of the inner
        // resolver's checks, including a deliberately wrong API family.
        *output = dlsym(RTLD_DEFAULT, wrapper);
        assert(*output);
        if (status)
            *status = 0;
        return 0;
    }
    if (getenv("CUINTERPOSE_TEST_NESTED_RUNTIME")) {
        // This is a public undefined CUDA symbol, intentionally resolved
        // through LD_PRELOAD rather than the driver's private fixture body.
        return cuGetProcAddress_v2(name, output, version, flags, status);
    }
    return cuFixtureQuery(name, output, version, flags, status);
}

int cudaGetDriverEntryPoint(const char *name, void **output, uint64_t flags, int *status) {
    return runtime_lookup(name, output, 13010, flags, status);
}

int cudaGetDriverEntryPoint_ptsz(const char *name, void **output, uint64_t flags, int *status) {
    return runtime_lookup(name, output, 13010, flags | 2, status);
}

int cudaGetDriverEntryPointByVersion(const char *name, void **output, unsigned version,
                                     uint64_t flags, int *status) {
    return runtime_lookup(name, output, (int)version, flags, status);
}

int cudaGetDriverEntryPointByVersion_ptsz(const char *name, void **output, unsigned version,
                                          uint64_t flags, int *status) {
    return runtime_lookup(name, output, (int)version, flags | 2, status);
}
