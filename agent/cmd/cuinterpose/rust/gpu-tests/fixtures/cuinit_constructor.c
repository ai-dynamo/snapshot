// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// cuInit runs while the dynamic loader holds its lock to run this constructor.
#define _GNU_SOURCE
// The assert makes the call under test, so it must not compile out.
#undef NDEBUG
#include <assert.h>
#include <dlfcn.h>

__attribute__((constructor)) static void initialize_in_constructor(void) {
    int (*initialize)(unsigned) = (int (*)(unsigned))dlsym(RTLD_DEFAULT, "cuInit");
    assert(initialize && initialize(0) == 0);
}
