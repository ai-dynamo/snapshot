// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include <dlfcn.h>
#include <stddef.h>

#ifndef MARKER
#define MARKER 17
#endif

int fixture_next_marker(void) {
    return MARKER;
}

#ifdef CALLER
int fixture_lookup_next(void) {
    // Prevent a compiler tail-call from changing the caller observed by dlsym.
    int (*next)(void) = dlsym(RTLD_NEXT, "fixture_next_marker");
    return next ? next() : -1;
}
#endif
