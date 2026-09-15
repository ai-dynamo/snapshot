// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Loaded only in a child whose parent already initialized the actual core.
// The worker must initialize a new process generation, not lazy-load a mock core.
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

struct stats {
    uint64_t allocations, handles, mappings, multicasts, exports, raw, unsupported;
    uint32_t phase;
};

static void (*debug_stats)(struct stats *);
static pthread_t worker;
static atomic_int worker_tid;
static struct stats worker_stats;
static int (*query)(const char *, void **, int, uint64_t);

static void *initialize_generation(void *unused) {
    (void)unused;
    atomic_store(&worker_tid, (int)syscall(SYS_gettid));
    debug_stats(&worker_stats);
    if (query) {
        void *output = NULL;
        assert(query("cuFixtureUnwrapped", &output, 13010, 0) == 0 && output);
    }
    return NULL;
}

__attribute__((constructor)) static void contend_with_generation_startup(void) {
    debug_stats = dlsym(RTLD_DEFAULT, "cuinterpose_debug_stats");
    assert(debug_stats);
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
        if (tid && access(endpoint, F_OK) == 0) {
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

    // A must not wait for B or expose partial state. Depending on the backend
    // runtime, B may already have completed initialization before blocking in
    // its subsequent resolver call. Both complete readiness and nonblocking
    // refusal are valid; debug_stats leaves output untouched on refusal.
    struct stats busy = {.phase = 99};
    debug_stats(&busy);
    assert(busy.phase == 99 || (busy.phase == 1 && busy.allocations == 0));
    if (query) {
        void *output = (void *)42;
        int status = query("cuFixtureUnwrapped", &output, 13010, 0);
        assert(status == (busy.phase == 99 ? 3 : 0));
        assert(busy.phase == 99 ? output == NULL : output != NULL);
    }
    int (*create)(uint64_t *, size_t, const void *, uint64_t) =
        dlsym(RTLD_DEFAULT, "cuMemCreate");
    assert(create);
    uint64_t handle = 42;
    assert(create(&handle, 4096, NULL, 0) == (busy.phase == 99 ? 3 : 1));
    assert(handle == 42);
}

void fixture_join_generation_worker(void) {
    // Called after dlopen returns and releases the loader lock needed by B.
    assert(pthread_join(worker, NULL) == 0);
    assert(worker_stats.phase == 1 && worker_stats.allocations == 0);
    struct stats ready = {0};
    debug_stats(&ready);
    assert(ready.phase == 1 && ready.allocations == 0);
}
