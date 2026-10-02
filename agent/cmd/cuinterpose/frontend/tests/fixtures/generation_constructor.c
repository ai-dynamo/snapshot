// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include <cuda.h>
#include <assert.h>
#include <dlfcn.h>

__attribute__((constructor)) static void initialize_generation(void) {
    CUresult (*initialize)(unsigned) = dlsym(RTLD_DEFAULT, "cuInit");
    assert(initialize && initialize(0) == CUDA_SUCCESS);
}
