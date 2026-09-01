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
#undef cuMulticastBindAddr
#undef cuMulticastBindMem

CUresult CUDAAPI cuGetProcAddress(const char*, void**, int, cuuint64_t);

extern CUresult CUDAAPI fakeCuMulticastCreateOriginal(
    CUmemGenericAllocationHandle*, const CUmulticastObjectProp*);

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
  size_t granularity = 0;

  require(
      cuMulticastCreate(&handle, NULL) == (CUresult)110 && handle == 0x456,
      "cuMulticastCreate");
  require(cuMulticastAddDevice(handle, 1) == (CUresult)111, "cuMulticastAddDevice");
  require(
      cuMulticastBindMem(handle, 2, 3, 4, 5, 0) == (CUresult)112,
      "cuMulticastBindMem");
  require(
      cuMulticastBindAddr(handle, 2, 3, 4, 0) == (CUresult)113,
      "cuMulticastBindAddr");
  require(
      cuMulticastGetGranularity(&granularity, NULL, 0) == (CUresult)114 &&
          granularity == 4096,
      "cuMulticastGetGranularity");
  require(cuMulticastUnbind(handle, 1, 2, 3) == (CUresult)115, "cuMulticastUnbind");
}

static void
test_resolvers(void)
{
  typedef CUresult(CUDAAPI * create_type)(
      CUmemGenericAllocationHandle*, const CUmulticastObjectProp*);
  void* cuda = dlopen("libcuda.so.1", RTLD_NOW);
  void* symbol = NULL;
  CUmemGenericAllocationHandle handle = 0;
  enum cudaDriverEntryPointQueryResult runtime_status;

  require(cuda != NULL, "dlopen libcuda");
  symbol = dlsym(cuda, "cuMulticastCreate");
  require(
      symbol != NULL && symbol != (void*)&fakeCuMulticastCreateOriginal,
      "dlsym substitution");
  require(
      ((create_type)symbol)(&handle, NULL) == (CUresult)110,
      "dlsym forwarding");

  symbol = NULL;
  require(
      cuGetProcAddress("cuMulticastCreate", &symbol, CUDA_VERSION, 0) == CUDA_SUCCESS,
      "cuGetProcAddress");
  require(
      symbol != NULL && symbol != (void*)&fakeCuMulticastCreateOriginal,
      "cuGetProcAddress substitution");

  symbol = NULL;
  require(
      cudaGetDriverEntryPoint(
          "cuMulticastCreate", &symbol, 0, &runtime_status) == cudaSuccess,
      "cudaGetDriverEntryPoint");
  require(
      runtime_status == cudaDriverEntryPointSuccess &&
          symbol != (void*)&fakeCuMulticastCreateOriginal,
      "runtime substitution");
  dlclose(cuda);
}

int
main(void)
{
  test_direct_forwarding();
  test_resolvers();
  puts("multicast forwarding OK");
  return 0;
}
