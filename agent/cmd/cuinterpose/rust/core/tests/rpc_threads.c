// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Test-only startup/pressure injection and deterministic lifecycle rendezvous.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

static atomic_int armed, refused, attempts, fail_nth;
static int ready_fd = -1, release_fd = -1;
static atomic_int block_copy, copy_entered, copy_release;

void rpc_fail_workers(int count) { atomic_store(&armed, count); }
int rpc_refused(void) { return atomic_load(&refused); }
int rpc_attempts(void) { return atomic_load(&attempts); }
void rpc_fail_startup(int nth) { atomic_store(&fail_nth, nth); }
void rpc_peer_barrier(int ready, int release) {
    ready_fd = ready;
    release_fd = release;
}
void rpc_block_copy(void) { atomic_store(&block_copy, 1); }
int rpc_copy_entered(void) { return atomic_load(&copy_entered); }
void rpc_release_copy(void) { atomic_store(&copy_release, 1); }

int connect(int fd, const struct sockaddr *address, socklen_t length) {
    // Only the lifecycle worker opens a peer connection while this rendezvous
    // is armed. Stop at that semantic boundary, not at packet byte offsets or
    // libc sendmsg (rustix may issue sendmsg directly as a Linux syscall).
    if (ready_fd >= 0 && address->sa_family == AF_UNIX &&
        length > offsetof(struct sockaddr_un, sun_path) &&
        memmem(((const struct sockaddr_un *)address)->sun_path,
               length - offsetof(struct sockaddr_un, sun_path),
               "/cuinterpose-", strlen("/cuinterpose-"))) {
        char token = 'R';
        if (write(ready_fd, &token, 1) != 1 ||
            read(release_fd, &token, 1) != 1)
            _exit(91);
        ready_fd = release_fd = -1;
    }
    int (*next)(int, const struct sockaddr *, socklen_t) = dlsym(RTLD_NEXT, "connect");
    return next(fd, address, length);
}

int cuMemcpyDtoHAsync_v2(void *host, uint64_t device, size_t size, void *stream) {
    if (atomic_load(&block_copy)) {
        atomic_store(&copy_entered, 1);
        while (!atomic_load(&copy_release)) usleep(1000);
    }
    int (*next)(void *, uint64_t, size_t, void *) = dlsym(RTLD_NEXT, "cuMemcpyDtoHAsync_v2");
    return next(host, device, size, stream);
}

int pthread_create(pthread_t *thread, const pthread_attr_t *attributes,
                   void *(*start)(void *), void *argument) {
    int attempt = atomic_fetch_add(&attempts, 1) + 1;
    if (attempt == atomic_load(&fail_nth)) {
        atomic_fetch_add(&refused, 1);
        return EAGAIN;
    }
    int remaining = atomic_load(&armed);
    while (remaining > 0) {
        if (atomic_compare_exchange_weak(&armed, &remaining, remaining - 1)) {
            atomic_fetch_add(&refused, 1);
            return EAGAIN;
        }
    }
    int (*next)(pthread_t *, const pthread_attr_t *, void *(*)(void *), void *) =
        dlsym(RTLD_NEXT, "pthread_create");
    return next(thread, attributes, start, argument);
}
