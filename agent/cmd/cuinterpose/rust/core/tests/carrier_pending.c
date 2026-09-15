// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// A copy whose completion stays unknown must terminate without freeing memory
// still accessible to the driver. Ordinary failure injection uses fakeFailNext.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdint.h>
#include <stddef.h>
#include <unistd.h>

static int armed, pending;
void carrier_pending_copies(void) { armed = 1; }

int cuMemcpyDtoHAsync_v2(void *host, uint64_t device, size_t size, void *stream) {
    if (armed) { pending = 1; return 0; }
    int (*next)(void *, uint64_t, size_t, void *) = dlsym(RTLD_NEXT, "cuMemcpyDtoHAsync_v2");
    return next(host, device, size, stream);
}

int cuMemcpyHtoDAsync_v2(uint64_t device, const void *host, size_t size, void *stream) {
    if (armed) { pending = 1; return 0; }
    int (*next)(uint64_t, const void *, size_t, void *) = dlsym(RTLD_NEXT, "cuMemcpyHtoDAsync_v2");
    return next(device, host, size, stream);
}

int cuStreamSynchronize(void *stream) {
    if (pending) return 711;
    int (*next)(void *) = dlsym(RTLD_NEXT, "cuStreamSynchronize");
    return next(stream);
}

// All cleanup enters one of these operations before releasing the staging
// resources or returning to the caller's context.
static void require_completed(void) {
    if (pending) {
        const char message[] = "UNSAFE pending-copy cleanup\n";
        (void)write(STDERR_FILENO, message, sizeof(message) - 1);
        _exit(78);
    }
}
int cuStreamDestroy_v2(void *stream) {
    require_completed();
    int (*next)(void *) = dlsym(RTLD_NEXT, "cuStreamDestroy_v2");
    return next(stream);
}
int cuMemUnmap(uint64_t address, size_t size) {
    require_completed();
    int (*next)(uint64_t, size_t) = dlsym(RTLD_NEXT, "cuMemUnmap");
    return next(address, size);
}
int cuCtxSetCurrent(void *context) {
    require_completed();
    int (*next)(void *) = dlsym(RTLD_NEXT, "cuCtxSetCurrent");
    return next(context);
}
