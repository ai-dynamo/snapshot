// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Calls linked directly against libcuda must bind to the preloaded wrappers.
#define _GNU_SOURCE
// The asserts make the calls under test, so they must not compile out.
#undef NDEBUG
#include <assert.h>
#include <dlfcn.h>
#include <stddef.h>
#include <string.h>

// The libcuda exports this program links against. cuda.h maps the unsuffixed API names
// to these _v2 exports.
typedef int CUresult;
typedef unsigned long long CUdeviceptr;
typedef struct CUctx_st *CUcontext;
CUresult cuInit(unsigned int flags);
CUresult cuDevicePrimaryCtxRetain(CUcontext *context, int device);
CUresult cuDevicePrimaryCtxRelease_v2(int device);
CUresult cuCtxSetCurrent(CUcontext context);
CUresult cuMemAlloc_v2(CUdeviceptr *address, size_t size);
CUresult cuMemFree_v2(CUdeviceptr address);
CUresult cuMemGetAddressRange_v2(CUdeviceptr *base, size_t *size, CUdeviceptr address);

static void in_shim(void *address, const char *name) {
    Dl_info info;
    assert(dladdr(address, &info));
    assert(strstr(info.dli_fname, "libcuinterpose.so"));
    assert(info.dli_sname && strcmp(info.dli_sname, name) == 0);
}

int main(void) {
    in_shim((void *)cuInit, "cuInit");
    in_shim((void *)cuMemAlloc_v2, "cuMemAlloc_v2");
    in_shim((void *)cuMemFree_v2, "cuMemFree_v2");
    in_shim((void *)cuMemGetAddressRange_v2, "cuMemGetAddressRange_v2");
    in_shim((void *)cuDevicePrimaryCtxRelease_v2, "cuDevicePrimaryCtxRelease_v2");
    assert(cuInit(0) == 0);
    CUcontext context;
    assert(cuDevicePrimaryCtxRetain(&context, 0) == 0);
    assert(cuCtxSetCurrent(context) == 0);
    CUdeviceptr allocation = 0, base = 0;
    size_t size = 0;
    assert(cuMemAlloc_v2(&allocation, 4096) == 0);
    assert(cuMemGetAddressRange_v2(&base, &size, allocation + 1) == 0);
    assert(base == allocation && size == 4096);
    assert(cuMemFree_v2(allocation) == 0);
    assert(cuCtxSetCurrent(NULL) == 0);
    assert(cuDevicePrimaryCtxRelease_v2(0) == 0);
    return 0;
}
