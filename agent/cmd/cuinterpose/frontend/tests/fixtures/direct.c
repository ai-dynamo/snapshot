// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#define _GNU_SOURCE
#include <cuda.h>
#include <assert.h>
#include <dlfcn.h>
#include <string.h>

int main(void) {
    Dl_info info;
    assert(dladdr((void *)cuMemAlloc, &info) != 0);
    assert(strstr(info.dli_fname, "libcuinterpose.so") != NULL);
    assert(cuInit(0) == CUDA_SUCCESS);
    int count = 0;
    assert(cuDeviceGetCount(&count) == CUDA_SUCCESS && count >= 2);
    for (int device = 0; device < 2; ++device) {
        CUcontext context;
        assert(cuDevicePrimaryCtxRetain(&context, device) == CUDA_SUCCESS);
        assert(cuCtxSetCurrent(context) == CUDA_SUCCESS);
        CUdeviceptr allocation = 0, base = 0;
        size_t size = 0;
        unsigned char bytes[4096];
        assert(cuMemAlloc(&allocation, sizeof(bytes)) == CUDA_SUCCESS);
        assert(cuMemGetAddressRange(&base, &size, allocation + 1) == CUDA_SUCCESS);
        assert(base == allocation && size == sizeof(bytes));
        assert(cuMemsetD8(allocation, 0x5a, sizeof(bytes)) == CUDA_SUCCESS);
        assert(cuMemcpyDtoH(bytes, allocation, sizeof(bytes)) == CUDA_SUCCESS);
        for (size_t i = 0; i < sizeof(bytes); ++i)
            assert(bytes[i] == 0x5a);
        assert(cuMemFree(allocation) == CUDA_SUCCESS);
        assert(cuCtxSetCurrent(NULL) == CUDA_SUCCESS);
        assert(cuDevicePrimaryCtxRelease(device) == CUDA_SUCCESS);
    }
    return 0;
}
