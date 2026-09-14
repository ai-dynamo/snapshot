// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "cuda.h"

// The unwrapped fixture query invokes the driver's own resolver body without
// accidentally calling the LD_PRELOAD wrapper again.
int cudaGetDriverEntryPoint(const char *name, void **output, uint64_t flags, int *status) {
    return cuFixtureQuery(name, output, 13010, flags, status);
}

int cudaGetDriverEntryPoint_ptsz(const char *name, void **output, uint64_t flags, int *status) {
    return cuFixtureQuery(name, output, 13010, flags | 2, status);
}

int cudaGetDriverEntryPointByVersion(const char *name, void **output, unsigned version,
                                     uint64_t flags, int *status) {
    return cuFixtureQuery(name, output, (int)version, flags, status);
}

int cudaGetDriverEntryPointByVersion_ptsz(const char *name, void **output, unsigned version,
                                          uint64_t flags, int *status) {
    return cuFixtureQuery(name, output, (int)version, flags | 2, status);
}
