// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "core_abi.h"
#include <assert.h>
#include <dlfcn.h>
#include <pthread.h>
#include <sys/mman.h>
#include <unistd.h>

static void *driver;
static pthread_barrier_t registration_start;
static CUresult (*initialize)(const struct FrontendAbi *, const struct BackendAbi **);
static void *resolve(const char *name) { return dlsym(driver, name); }
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
    assert(argc == 2);
    driver = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
    void *core = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
    assert(driver && core);
    initialize = dlsym(core, "cuinterpose_core_init");
    assert(initialize);
    const struct BackendAbi *output = NULL;
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
    // Both libraries remain loaded while the registered callbacks are available.
    return 0;
}
