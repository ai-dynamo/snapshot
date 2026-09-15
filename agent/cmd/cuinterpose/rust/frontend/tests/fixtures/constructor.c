// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include "cuda.h"
#include <assert.h>
#include <errno.h>
#include <pthread.h>
#include <sched.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

extern create_fn fixture_create;
static atomic_int worker_tid;
static pthread_t worker;

static void *create_from_worker(void *unused) {
    (void)unused;
    atomic_store(&worker_tid, (int)syscall(SYS_gettid));
    uint64_t handle = 0;
    assert(fixture_create(&handle, 4096, NULL, 0) == 0);
    assert(handle == 0xabcdef);
    return NULL;
}

__attribute__((constructor)) static void contend_with_loader(void) {
    assert(pthread_create(&worker, NULL, create_from_worker, NULL) == 0);
    // dlopen runs us under glibc's loader lock. Wait until B is sleeping in
    // futex after entering create, not an arbitrary delay or scheduler guess.
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
    if (strcmp(getenv("CUINTERPOSE_TEST_CONSTRUCTOR"), "fork") == 0) {
        errno = 0;
        assert(fork() == -1 && errno == EAGAIN);
    } else {
        uint64_t handle = 42;
        // A must not wait for B's core initialization while holding the lock
        // needed by B. The failed attempt must not poison B's eventual result.
        assert(fixture_create(&handle, 4096, NULL, 0) == 3);
        assert(handle == 42);
    }
}

void fixture_join_worker(void) {
    assert(pthread_join(worker, NULL) == 0);
}
