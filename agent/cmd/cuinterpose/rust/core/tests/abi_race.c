// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include <core_abi.h>
#include <assert.h>
#include <dlfcn.h>
#include <pthread.h>
#include <stdio.h>
#include <unistd.h>

static pthread_barrier_t barrier;
static CUresult (*initialize)(const struct FrontendAbi *, const struct BackendAbi **);
static struct FrontendAbi frontend;
static const struct BackendAbi *tables[32];
static void *resolve(const char *name) { (void)name; return NULL; }
static void *worker(void *index) {
    size_t i = (size_t)index;
    pthread_barrier_wait(&barrier);
    assert(initialize(&frontend, &tables[i]) == CUDA_SUCCESS);
    return NULL;
}
int main(int argc, char **argv) {
    assert(argc == 2);
    void *library = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    assert(library);
    initialize = dlsym(library, "cuinterpose_core_init");
    assert(initialize);
    frontend = (struct FrontendAbi){ABI_VERSION, sizeof(frontend), resolve, getpid()};
    assert(pthread_barrier_init(&barrier, NULL, 32) == 0);
    pthread_t threads[32];
    for (size_t i = 0; i < 32; ++i)
        assert(pthread_create(&threads[i], NULL, worker, (void *)i) == 0);
    for (size_t i = 0; i < 32; ++i) {
        assert(pthread_join(threads[i], NULL) == 0);
        assert(tables[i] && tables[i] == tables[0]);
    }
    puts("PASS ABI handshake: all 32 callers succeed with the same table");
}
