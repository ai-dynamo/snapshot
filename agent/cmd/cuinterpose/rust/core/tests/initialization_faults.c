// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Faults are confined to this test preload; production has no test exports.
#define _GNU_SOURCE
#include <assert.h>
#include <dlfcn.h>
#include <errno.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

static int (*real_create)(pthread_t *, const pthread_attr_t *, void *(*)(void *), void *);
static int (*initialize)(unsigned);
static _Thread_local const char *mode;
static _Thread_local int attempts;
static atomic_int entered, release_call, release_worker, recursive_result;
#define NEXT(name) \
    ((void *(*)(void *, const char *))dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34"))(RTLD_NEXT, name)
struct parked { void *(*start)(void *); void *argument; };
__attribute__((constructor)) static void setup(void) {
    real_create = NEXT("pthread_create");
    initialize = dlsym(RTLD_DEFAULT, "cuInit");
    assert(real_create && initialize);
}
int fault_entered(void) { return atomic_load(&entered); }
void fault_release(void) { atomic_store(&release_call, 1); }
void fault_release_workers(void) { atomic_store(&release_worker, 1); }
int fault_recursive_result(void) { return atomic_load(&recursive_result); }
int fault_call(const char *selected) {
    mode = selected;
    attempts = 0;
    int result = initialize(0);
    mode = NULL;
    return result;
}
static void *delayed(void *pointer) {
    struct parked copy = *(struct parked *)pointer;
    free(pointer);
    while (!atomic_load(&release_worker)) usleep(1000);
    return copy.start(copy.argument);
}
int pthread_create(pthread_t *thread, const pthread_attr_t *attr,
                   void *(*start)(void *), void *argument) {
    if (mode) {
        int attempt = ++attempts;
        if (strcmp(mode, "failure-race") == 0 && attempt == 1) {
            atomic_store(&entered, 1);
            while (!atomic_load(&release_call)) usleep(1000);
            return EAGAIN;
        }
        if (strcmp(mode, "delayed") == 0) {
            if (attempt == 2) {
                atomic_store(&entered, 1);
                while (!atomic_load(&release_call)) usleep(1000);
            }
            struct parked *parked = malloc(sizeof(*parked));
            assert(parked);
            *parked = (struct parked){start, argument};
            return real_create(thread, attr, delayed, parked);
        }
        if (strcmp(mode, "recursive") == 0 && attempt == 1)
            atomic_store(&recursive_result, initialize(0));
        if ((strcmp(mode, "first-spawn") == 0 && attempt == 1) ||
            (strcmp(mode, "second-spawn") == 0 && attempt == 2))
            return EAGAIN;
    }
    return real_create(thread, attr, start, argument);
}
int chmod(const char *path, mode_t bits) {
    if (getenv("CUINTERPOSE_TEST_CHMOD_FAILURE")) {
        errno = EPERM;
        return -1;
    }
    int (*real)(const char *, mode_t) = NEXT("chmod");
    return real(path, bits);
}
