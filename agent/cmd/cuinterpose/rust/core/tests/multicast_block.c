// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Test-only provider layer: block a collective CUDA call and inject one-shot
// driver failures, exposing concurrent control and ownership paths.
#define _GNU_SOURCE
#include "fixtures/next.h"
#include <dlfcn.h>
#include <pthread.h>
#include <stdint.h>
#include <stddef.h>
#include <stdatomic.h>

static pthread_mutex_t lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t changed = PTHREAD_COND_INITIALIZER;
static int armed, entered, released;
static atomic_int access_failure;
static atomic_int create_failure;

void multicast_fail_access(void) {
    atomic_store(&access_failure, 1);
}

void multicast_fail_create(void) {
    atomic_store(&create_failure, 1);
}

int cuMulticastCreate(uint64_t *handle, const void *properties) {
    if (atomic_exchange(&create_failure, 0)) {
        // The pinned forwarding driver writes this output even on failure.
        *handle = 0x456;
        return 110;
    }
    void *(*original)(const char *) = NEXT("fakeOriginal");
    int (*next)(uint64_t *, const void *) =
        original ? original("cuMulticastCreate") : 0;
    return next ? next(handle, properties) : 3;
}

int cuMemSetAccess(uint64_t address, size_t size, const void *descriptors, size_t count) {
    if (atomic_exchange(&access_failure, 0)) return 999;
    void *(*original)(const char *) = NEXT("fakeOriginal");
    int (*next)(uint64_t, size_t, const void *, size_t) =
        original ? original("cuMemSetAccess") : 0;
    return next ? next(address, size, descriptors, count) : 3;
}

void multicast_block_arm(int operation) {
    pthread_mutex_lock(&lock);
    armed = operation;
    entered = released = 0;
    pthread_mutex_unlock(&lock);
}

void multicast_block_wait(void) {
    pthread_mutex_lock(&lock);
    while (!entered) pthread_cond_wait(&changed, &lock);
    pthread_mutex_unlock(&lock);
}

void multicast_block_release(void) {
    pthread_mutex_lock(&lock);
    released = 1;
    pthread_cond_broadcast(&changed);
    pthread_mutex_unlock(&lock);
}

int cuMulticastAddDevice(uint64_t handle, int device) {
    pthread_mutex_lock(&lock);
    if (armed == 1) {
        armed = 0;
        entered = 1;
        pthread_cond_broadcast(&changed);
        while (!released) pthread_cond_wait(&changed, &lock);
    }
    pthread_mutex_unlock(&lock);
    int (*next)(uint64_t, int) = NEXT("cuMulticastAddDevice");
    return next ? next(handle, device) : 3;
}
