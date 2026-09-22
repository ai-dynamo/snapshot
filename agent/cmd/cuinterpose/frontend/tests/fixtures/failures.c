// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Faults affect only an explicitly armed test thread, never loader internals.
#define _GNU_SOURCE
#include <assert.h>
#include <dlfcn.h>
#include <stdatomic.h>
#include <string.h>
#include <unistd.h>

static void *(*real_dlopen)(const char *, int);
static _Thread_local const char *fault;
static atomic_bool entered, released;

__attribute__((constructor)) static void setup(void) {
    void *(*original)(void *, const char *) = dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34");
    assert(original);
    real_dlopen = original(RTLD_NEXT, "dlopen");
    assert(real_dlopen);
}

void fixture_fail_load(const char *mode) { fault = mode; }
int fixture_load_entered(void) { return atomic_load(&entered); }
void fixture_release_load(void) { atomic_store(&released, 1); }

void *dlopen(const char *path, int flags) {
    if (fault && path) {
        const char *base = strrchr(path, '/');
        base = base ? base + 1 : path;
        int backend = strcmp(base, "libcuinterpose_core.so") == 0;
        int retention = (flags & RTLD_NOLOAD) && strcmp(base, "libcuda.so.1") == 0;
        if ((backend && strcmp(fault, "retention") != 0) ||
            (retention && strcmp(fault, "retention") == 0)) {
            int late_winner = strcmp(fault, "backend-late-winner") == 0;
            int race = late_winner || strcmp(fault, "backend-race") == 0;
            fault = NULL;
            if (race) {
                atomic_store(&entered, 1);
                while (!atomic_load(&released))
                    usleep(1000);
            }
            return late_winner ? real_dlopen(path, flags) : NULL;
        }
    }
    return real_dlopen(path, flags);
}
