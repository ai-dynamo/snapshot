// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Test-only CUDA layer. Every normal call uses genuine RTLD_NEXT chaining.
// Zero handles are translated only here because the pinned model starts at 0x1000.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdint.h>
#include <stddef.h>
#include <string.h>
#include <unistd.h>

enum { CREATE, MAP, ACCESS, D2H, H2D, SYNC, DESTROY, UNMAP, FREE,
       RELEASE, PROPERTIES, SET_CONTEXT, RETAIN_PRIMARY, RELEASE_PRIMARY,
       REGISTER, UNREGISTER, RETAIN, OPERATIONS };
static int calls[OPERATIONS], fail_at[OPERATIONS], fail_after[OPERATIONS];
static int fail_code[OPERATIONS], delays[OPERATIONS];
static uint64_t zero_real;
static int zero_enabled, fresh_zero, fresh_zero_import;
static int streams, reservations;
static int pending, persistent_sync;

void carrier_reset(void) {
    memset(calls, 0, sizeof(calls));
    memset(fail_at, 0, sizeof(fail_at));
    memset(delays, 0, sizeof(delays));
}
void carrier_fail(int op, int nth, int after, int code) {
    fail_at[op] = nth; fail_after[op] = after; fail_code[op] = code;
}
void carrier_delay(int op, int microseconds) { delays[op] = microseconds; }
int carrier_calls(int op) { return calls[op]; }
int carrier_streams(void) { return streams; }
int carrier_reservations(void) { return reservations; }
int carrier_pending(void) { return pending; }
void carrier_pending_copies(void) { persistent_sync = 1; }
uint64_t carrier_zero_real(void) { return zero_real; }
void carrier_zero(void) { zero_enabled = fresh_zero = 1; }
void carrier_zero_import(void) { zero_enabled = fresh_zero_import = 1; }

static int before(int op) {
    if (pending && (op == RELEASE || op == UNMAP || op == FREE || op == DESTROY ||
                    op == SET_CONTEXT || op == RELEASE_PRIMARY || op == UNREGISTER)) {
        const char message[] = "UNSAFE pending-copy cleanup\n";
        (void)write(STDERR_FILENO, message, sizeof(message) - 1);
        _exit(78);
    }
    ++calls[op];
    if (delays[op]) usleep(delays[op]);
    return calls[op] == fail_at[op] && !fail_after[op] ? fail_code[op] : 0;
}
static int after(int op, int result) {
    return result == 0 && calls[op] == fail_at[op] && fail_after[op] ? fail_code[op] : result;
}
static uint64_t real_handle(uint64_t handle) {
    return zero_enabled && handle == 0 ? zero_real : handle;
}

int cuMemCreate(uint64_t *out, size_t size, const void *prop, uint64_t flags) {
    int error = before(CREATE);
    if (error) return error;
    int (*next)(uint64_t *, size_t, const void *, uint64_t) = dlsym(RTLD_NEXT, "cuMemCreate");
    int result = next(out, size, prop, flags);
    if (!result && fresh_zero) {
        zero_real = *out; *out = 0; fresh_zero = 0;
    }
    return after(CREATE, result);
}
int cuMemRelease(uint64_t handle) {
    int error = before(RELEASE);
    if (error) return error;
    int (*next)(uint64_t) = dlsym(RTLD_NEXT, "cuMemRelease");
    return after(RELEASE, next(real_handle(handle)));
}
int cuMemMap(uint64_t addr, size_t size, size_t offset, uint64_t handle, uint64_t flags) {
    int error = before(MAP);
    if (error) return error;
    int (*next)(uint64_t, size_t, size_t, uint64_t, uint64_t) = dlsym(RTLD_NEXT, "cuMemMap");
    return after(MAP, next(addr, size, offset, real_handle(handle), flags));
}
int cuMemUnmap(uint64_t addr, size_t size) {
    int error = before(UNMAP);
    if (error) return error;
    int (*next)(uint64_t, size_t) = dlsym(RTLD_NEXT, "cuMemUnmap");
    return after(UNMAP, next(addr, size));
}
int cuMemSetAccess(uint64_t addr, size_t size, const void *access, size_t count) {
    int error = before(ACCESS);
    if (error) return error;
    int (*next)(uint64_t, size_t, const void *, size_t) = dlsym(RTLD_NEXT, "cuMemSetAccess");
    return after(ACCESS, next(addr, size, access, count));
}
int cuMemGetAllocationPropertiesFromHandle(void *out, uint64_t handle) {
    int error = before(PROPERTIES);
    if (error) return error;
    int (*next)(void *, uint64_t) = dlsym(RTLD_NEXT, "cuMemGetAllocationPropertiesFromHandle");
    return after(PROPERTIES, next(out, real_handle(handle)));
}
int cuMemExportToShareableHandle(void *out, uint64_t handle, unsigned kind, uint64_t flags) {
    int (*next)(void *, uint64_t, unsigned, uint64_t) = dlsym(RTLD_NEXT, "cuMemExportToShareableHandle");
    return next(out, real_handle(handle), kind, flags);
}
int cuMemImportFromShareableHandle(uint64_t *out, void *fd, unsigned kind) {
    int (*next)(uint64_t *, void *, unsigned) = dlsym(RTLD_NEXT, "cuMemImportFromShareableHandle");
    int result = next(out, fd, kind);
    if (!result && fresh_zero_import) {
        zero_real = *out; fresh_zero_import = 0;
    }
    if (!result && zero_enabled && *out == zero_real) *out = 0;
    return result;
}
int cuMemRetainAllocationHandle(uint64_t *out, void *address) {
    int error = before(RETAIN);
    if (error) return error;
    int (*next)(uint64_t *, void *) = dlsym(RTLD_NEXT, "cuMemRetainAllocationHandle");
    int result = next(out, address);
    if (!result && zero_enabled && *out == zero_real) *out = 0;
    return after(RETAIN, result);
}
int cuMemAddressReserve(uint64_t *out, size_t size, size_t alignment, uint64_t fixed, uint64_t flags) {
    int (*next)(uint64_t *, size_t, size_t, uint64_t, uint64_t) = dlsym(RTLD_NEXT, "cuMemAddressReserve");
    int result = next(out, size, alignment, fixed, flags);
    if (!result) ++reservations;
    return result;
}
int cuMemAddressFree(uint64_t addr, size_t size) {
    int error = before(FREE);
    if (error) return error;
    int (*next)(uint64_t, size_t) = dlsym(RTLD_NEXT, "cuMemAddressFree");
    int result = next(addr, size);
    if (!result) --reservations;
    return after(FREE, result);
}
int cuStreamCreate(void **out, unsigned flags) {
    int (*next)(void **, unsigned) = dlsym(RTLD_NEXT, "cuStreamCreate");
    int result = next(out, flags);
    if (!result) ++streams;
    return result;
}
int cuStreamSynchronize(void *stream) {
    int error = before(SYNC);
    if (pending && persistent_sync) return calls[SYNC] == 1 ? 711 : 712;
    if (error) return error;
    int (*next)(void *) = dlsym(RTLD_NEXT, "cuStreamSynchronize");
    return after(SYNC, next(stream));
}
int cuStreamDestroy_v2(void *stream) {
    int error = before(DESTROY);
    if (error) return error;
    int (*next)(void *) = dlsym(RTLD_NEXT, "cuStreamDestroy_v2");
    int result = next(stream);
    if (!result) --streams;
    return after(DESTROY, result);
}
int cuMemcpyDtoHAsync_v2(void *host, uint64_t device, size_t size, void *stream) {
    int error = before(D2H);
    if (error) return error;
    if (persistent_sync) { ++pending; return 0; }
    int (*next)(void *, uint64_t, size_t, void *) = dlsym(RTLD_NEXT, "cuMemcpyDtoHAsync_v2");
    return after(D2H, next(host, device, size, stream));
}
int cuMemcpyHtoDAsync_v2(uint64_t device, const void *host, size_t size, void *stream) {
    int error = before(H2D);
    if (error) return error;
    if (persistent_sync) { ++pending; return 0; }
    int (*next)(uint64_t, const void *, size_t, void *) = dlsym(RTLD_NEXT, "cuMemcpyHtoDAsync_v2");
    return after(H2D, next(device, host, size, stream));
}
int cuCtxSetCurrent(void *context) {
    int error = before(SET_CONTEXT);
    if (error) return error;
    int (*next)(void *) = dlsym(RTLD_NEXT, "cuCtxSetCurrent");
    return after(SET_CONTEXT, next(context));
}
int cuDevicePrimaryCtxRetain(void **out, int device) {
    int error = before(RETAIN_PRIMARY);
    if (error) return error;
    int (*next)(void **, int) = dlsym(RTLD_NEXT, "cuDevicePrimaryCtxRetain");
    return after(RETAIN_PRIMARY, next(out, device));
}
int cuDevicePrimaryCtxRelease_v2(int device) {
    int error = before(RELEASE_PRIMARY);
    if (error) return error;
    int (*next)(int) = dlsym(RTLD_NEXT, "cuDevicePrimaryCtxRelease_v2");
    return after(RELEASE_PRIMARY, next(device));
}
int cuMemHostRegister_v2(void *base, size_t size, unsigned flags) {
    int error = before(REGISTER);
    if (error) return error;
    int (*next)(void *, size_t, unsigned) = dlsym(RTLD_NEXT, "cuMemHostRegister_v2");
    return after(REGISTER, next(base, size, flags));
}
int cuMemHostUnregister(void *base) {
    int error = before(UNREGISTER);
    if (error) return error;
    int (*next)(void *) = dlsym(RTLD_NEXT, "cuMemHostUnregister");
    return after(UNREGISTER, next(base));
}
int cuMulticastBindMem_v2(uint64_t handle, int device, size_t offset,
                         uint64_t member, size_t member_offset, size_t size, uint64_t flags) {
    int (*next)(uint64_t, int, size_t, uint64_t, size_t, size_t, uint64_t) =
        dlsym(RTLD_NEXT, "cuMulticastBindMem_v2");
    return next(handle, device, offset, real_handle(member), member_offset, size, flags);
}
