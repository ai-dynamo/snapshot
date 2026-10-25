// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Preloaded after the frontend, so the shim's driver lookups resolve cuMemSetAccess here.
// Once armed, the nth access grant applies natively and then reports failure, because
// CUDA does not promise that a failed access update changed no pages.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdatomic.h>
#include <stddef.h>

typedef int CUresult;
typedef unsigned long long CUdeviceptr;
typedef struct {
    int type;
    int id;
    int flags;
} CUmemAccessDesc;
typedef CUresult (*SetAccess)(CUdeviceptr, size_t, const CUmemAccessDesc *, size_t);
typedef void *(*Dlsym)(void *, const char *);

enum {
    CUDA_ERROR_OUT_OF_MEMORY = 2,
    CUDA_ERROR_NOT_INITIALIZED = 3,
    CU_MEM_ACCESS_FLAGS_PROT_NONE = 0,
};

static atomic_uint remaining_grants;

// Fails the nth following call that grants access.
void fail_grant(unsigned n) {
    atomic_store(&remaining_grants, n);
}

// The test loads libcuda privately, so RTLD_NEXT cannot reach it. The frontend's dlsym
// would return its own wrapper for the libcuda handle, so use glibc's.
static SetAccess driver_set_access(void) {
    void *driver = dlopen("libcuda.so.1", RTLD_LAZY | RTLD_NOLOAD);
    Dlsym lookup = (Dlsym)dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34");
    return driver && lookup ? (SetAccess)lookup(driver, "cuMemSetAccess") : NULL;
}

CUresult cuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc *access,
                        size_t count) {
    SetAccess set_access = driver_set_access();
    if (!set_access)
        return CUDA_ERROR_NOT_INITIALIZED;
    CUresult result = set_access(address, size, access, count);
    if (result != 0 || count == 0 || access[0].flags == CU_MEM_ACCESS_FLAGS_PROT_NONE)
        return result;
    unsigned remaining = atomic_load(&remaining_grants);
    while (remaining && !atomic_compare_exchange_weak(&remaining_grants, &remaining,
                                                      remaining - 1))
        ;
    return remaining == 1 ? CUDA_ERROR_OUT_OF_MEMORY : result;
}
