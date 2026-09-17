// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Fault injectors delegate directly through glibc, not the frontend's dlsym.
// Keep the real call in this DSO: RTLD_NEXT is relative to its return address.
#include <dlfcn.h>
#define NEXT(name) \
    ((void *(*)(void *, const char *))dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34"))(RTLD_NEXT, name)
