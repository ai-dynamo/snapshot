/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include <cuda.h>
#include <cuda_runtime_api.h>
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>

#undef cuGetProcAddress

CUresult CUDAAPI cuGetProcAddress(const char*, void**, int, cuuint64_t);

extern CUresult CUDAAPI fakeCuMemCreateOriginal(
    CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);

static void
require(int condition, const char* message)
{
  if (!condition) {
    fprintf(stderr, "FAIL: %s\n", message);
    exit(1);
  }
}

static void
test_direct_forwarding(void)
{
  CUmemGenericAllocationHandle handle = 0;

  require(cuMemCreate(&handle, 1, NULL, 0) == (CUresult)101 && handle == 0xabc, "cuMemCreate");
  require(cuMemRelease(handle) == (CUresult)102, "cuMemRelease");
  require(cuMemRetainAllocationHandle(&handle, NULL) == (CUresult)103 && handle == 0xdef, "cuMemRetain");
  require(cuMemMap(1, 2, 3, handle, 0) == (CUresult)104, "cuMemMap");
  require(cuMemUnmap(1, 2) == (CUresult)105, "cuMemUnmap");
  require(cuMemSetAccess(1, 2, NULL, 0) == (CUresult)106, "cuMemSetAccess");
  require(cuMemExportToShareableHandle(NULL, handle, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0) == (CUresult)107,
          "cuMemExport");
  require(cuMemImportFromShareableHandle(&handle, NULL, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR) == (CUresult)108 &&
              handle == 0x123,
          "cuMemImport");
  require(cuMemGetAllocationPropertiesFromHandle(NULL, handle) == (CUresult)109, "cuMemGetProperties");
}

static void
test_resolvers(void)
{
  typedef CUresult(CUDAAPI * create_type)(
      CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);
  void* cuda = dlopen("libcuda.so.1", RTLD_NOW);
  void* symbol = NULL;
  CUmemGenericAllocationHandle handle = 0;
  enum cudaDriverEntryPointQueryResult runtime_status;

  require(cuda != NULL, "dlopen libcuda");
  symbol = dlsym(cuda, "cuMemCreate");
  require(symbol != NULL && symbol != (void*)&fakeCuMemCreateOriginal, "dlsym substitution");
  require(((create_type)symbol)(&handle, 1, NULL, 0) == (CUresult)101, "dlsym forwarding");

  symbol = NULL;
  require(cuGetProcAddress("cuMemCreate", &symbol, CUDA_VERSION, 0) == CUDA_SUCCESS, "cuGetProcAddress");
  require(symbol != NULL && symbol != (void*)&fakeCuMemCreateOriginal, "cuGetProcAddress substitution");

  symbol = NULL;
  require(cudaGetDriverEntryPoint("cuMemCreate", &symbol, 0, &runtime_status) == cudaSuccess,
          "cudaGetDriverEntryPoint");
  require(runtime_status == cudaDriverEntryPointSuccess && symbol != (void*)&fakeCuMemCreateOriginal,
          "runtime substitution");
  dlclose(cuda);
}

int
main(void)
{
  test_direct_forwarding();
  test_resolvers();
  puts("forwarding OK");
  return 0;
}
