// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "core_abi.h"
#include <assert.h>
#include <dlfcn.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

static void *driver;
static const char *missing;
static pthread_barrier_t registration_start;
static CUresult (*initialize)(const struct FrontendAbi *, const struct BackendAbi **);
static void *resolve(const char *name) {
    if (missing && (!strcmp(name, missing) ||
                    (!strcmp(missing, "multicast") && !strncmp(name, "cuMulticast", 11))))
        return NULL;
    return dlsym(driver, name);
}
static void *different_resolver(const char *name) { return resolve(name); }
static const struct FrontendAbi frontend = {ABI_VERSION, sizeof(struct FrontendAbi), resolve};

static void *register_frontend(void *unused) {
    (void)unused;
    int barrier = pthread_barrier_wait(&registration_start);
    assert(barrier == 0 || barrier == PTHREAD_BARRIER_SERIAL_THREAD);
    const struct BackendAbi *backend = NULL;
    assert(initialize(&frontend, &backend) == CUDA_SUCCESS);
    assert(backend && backend->version == ABI_VERSION && backend->size == sizeof(*backend));
    return (void *)backend;
}

int main(int argc, char **argv) {
    assert(argc == 2 || argc == 3);
    driver = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
    void *core = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    assert(driver && core);
    initialize = dlsym(core, "cuinterpose_core_init");
    assert(initialize);
    const struct BackendAbi *output = NULL;
    if (argc == 3) {
        missing = argv[2];
        assert(!strcmp(missing, "cuMemCreate") || !strcmp(missing, "multicast"));
        CUresult (*cuda_init)(unsigned int) = dlsym(driver, "cuInit");
        assert(cuda_init && cuda_init(0) == CUDA_SUCCESS);
        assert(initialize(&frontend, &output) == CUDA_SUCCESS);
        CUresult status = output->ensure_cuinterpose_initialized();
        char endpoint[4096];
        assert(snprintf(endpoint, sizeof(endpoint), "%s/cuinterpose-%ld.sock",
                        getenv("CUINTERPOSE_SOCKET_DIR"), (long)getpid()) > 0);
        if (!strcmp(missing, "cuMemCreate")) {
            assert(status == CUDA_ERROR_NOT_INITIALIZED);
            assert(access(endpoint, F_OK) != 0);
        } else {
            assert(status == CUDA_SUCCESS && access(endpoint, F_OK) == 0);
            CUcontext context;
            CUresult (*retain)(CUcontext *, CUdevice) = dlsym(driver, "cuDevicePrimaryCtxRetain");
            CUresult (*set_current)(CUcontext) = dlsym(driver, "cuCtxSetCurrent");
            assert(retain && set_current && retain(&context, 0) == CUDA_SUCCESS);
            assert(set_current(context) == CUDA_SUCCESS);
            CUdeviceptr allocation;
            assert(output->cuMemAlloc_v2(&allocation, 4096) == CUDA_SUCCESS);
            assert(output->cuMemFree_v2(allocation) == CUDA_SUCCESS);
            assert(output->cuDevicePrimaryCtxRelease_v2(0) == CUDA_SUCCESS);
        }
        return 0;
    }
    assert(initialize(NULL, &output) == CUDA_ERROR_INVALID_VALUE && !output);
    assert(initialize(&frontend, NULL) == CUDA_ERROR_INVALID_VALUE);
    long page_size = sysconf(_SC_PAGESIZE);
    assert(page_size > 0);
    char *pages = mmap(NULL, 2 * page_size, PROT_READ | PROT_WRITE,
                       MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(pages != MAP_FAILED);
    assert(mprotect(pages + page_size, page_size, PROT_NONE) == 0);
    // An invalid prefix must be rejected before reading the remaining ABI.
    uint32_t *prefix = (void *)(pages + page_size - 2 * sizeof(uint32_t));
    prefix[0] = ABI_VERSION + 1;
    prefix[1] = sizeof(frontend);
    assert(initialize((const struct FrontendAbi *)prefix, &output) == CUDA_ERROR_INVALID_VALUE);
    prefix[0] = ABI_VERSION;
    prefix[1] = 2 * sizeof(uint32_t);
    assert(initialize((const struct FrontendAbi *)prefix, &output) == CUDA_ERROR_INVALID_VALUE);
    assert(!output);
    assert(munmap(pages, 2 * page_size) == 0);
    pthread_t threads[16];
    assert(pthread_barrier_init(&registration_start, NULL, 16) == 0);
    for (unsigned i = 0; i < 16; ++i)
        assert(pthread_create(&threads[i], NULL, register_frontend, NULL) == 0);
    for (unsigned i = 0; i < 16; ++i) {
        void *backend;
        assert(pthread_join(threads[i], &backend) == 0);
        if (output)
            assert(backend == output);
        output = backend;
    }
    assert(pthread_barrier_destroy(&registration_start) == 0);
    const struct BackendAbi *again = NULL;
    assert(initialize(&frontend, &again) == CUDA_SUCCESS && again == output);
    const struct FrontendAbi different = {ABI_VERSION, sizeof(frontend), different_resolver};
    again = NULL;
    assert(initialize(&different, &again) == CUDA_ERROR_INVALID_VALUE && !again);
    assert(initialize(&frontend, &again) == CUDA_SUCCESS && again == output);
    // Both libraries remain loaded while the registered callbacks are available.
    return 0;
}
