// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Loaded in a cold process or a child whose parent initialized the actual core.
// The worker initializes a process generation, not a mock backend.
#define _GNU_SOURCE
#include <assert.h>
#include <dlfcn.h>
#include <pthread.h>
#include <sched.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

static pthread_t worker;
static atomic_int worker_tid;
static int worker_result = -1;
static int (*create)(uint64_t *, size_t, const void *, uint64_t);
static int (*query)(const char *, void **, int, uint64_t);

static void *initialize_generation(void *unused) {
    (void)unused;
    atomic_store(&worker_tid, (int)syscall(SYS_gettid));
    uint64_t handle = 42;
    worker_result = create(&handle, 4096, NULL, 0);
    assert(handle == 42);
    if (query) {
        void *output = NULL;
        assert(query("cuFixtureUnwrapped", &output, 13010, 0) == 0 && output);
    }
    return NULL;
}

__attribute__((constructor)) static void contend_with_generation_startup(void) {
    create = dlsym(RTLD_DEFAULT, "cuMemCreate");
    assert(create);
    if (getenv("CUINTERPOSE_TEST_GENERATION_QUERY")) {
        query = dlsym(RTLD_DEFAULT, "cuGetProcAddress");
        assert(query);
    }
    assert(pthread_create(&worker, NULL, initialize_generation, NULL) == 0);

    char endpoint[256];
    snprintf(endpoint, sizeof(endpoint), "%s/cuinterpose-%d.sock",
             getenv("SNAPSHOT_CONTROL_DIR"), getpid());
    const time_t deadline = time(NULL) + 5;
    for (;;) {
        assert(time(NULL) < deadline);
        int tid = atomic_load(&worker_tid);
        if (tid) {
            char path[128], state[128] = {0};
            snprintf(path, sizeof(path), "/proc/self/task/%d/syscall", tid);
            FILE *file = fopen(path, "r");
            assert(file);
            assert(fgets(state, sizeof(state), file));
            fclose(file);
            char *end;
            long number = strtol(state, &end, 10);
            if (end != state && number == SYS_futex)
                break;
        }
        sched_yield();
    }

    // B is preparing private workers and has not bound the canonical endpoint.
    // A owns the loader lock and must be able to prepare and install its own
    // candidate, rather than waiting for B's loader-sensitive work.
    assert(access(endpoint, F_OK) != 0);
    if (query) {
        void *output = (void *)42;
        assert(query("cuFixtureUnwrapped", &output, 13010, 0) == 0);
        assert(output != NULL);
    }
    uint64_t handle = 42;
    assert(create(&handle, 4096, NULL, 0) == 1); // CUDA_ERROR_INVALID_VALUE
    assert(handle == 42);
}

void fixture_join_generation_worker(void) {
    // Called after dlopen returns and releases the loader lock needed by B.
    assert(pthread_join(worker, NULL) == 0);
    assert(worker_result == 1); // CUDA_ERROR_INVALID_VALUE after initialization.
}
