/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include <cuda.h>
#include <cuda_runtime_api.h>
#include <dlfcn.h>
#include <stdint.h>
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
require_logical_handle(CUmemGenericAllocationHandle handle, const char* message)
{
  require(
      ((uint64_t)handle & UINT64_C(0xffff000000000000)) ==
          UINT64_C(0xd94d000000000000),
      message);
}

static CUmulticastObjectProp
properties(void)
{
  CUmulticastObjectProp value = {0};

  value.numDevices = 1;
  value.size = 4096;
  value.handleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  return value;
}

static void
test_tracked_behavior(void)
{
  CUmemGenericAllocationHandle handle = 0;
  CUmulticastObjectProp props = properties();
  size_t granularity = 0;

  require(cuMulticastCreate(&handle, &props) == CUDA_SUCCESS, "cuMulticastCreate");
  require_logical_handle(handle, "cuMulticastCreate logical handle");
  require(cuMulticastAddDevice(handle, 1) == (CUresult)111, "cuMulticastAddDevice");
  require(
      cuMulticastGetGranularity(&granularity, NULL, 0) == (CUresult)114 &&
          granularity == 4096,
      "cuMulticastGetGranularity");
}

static void
test_resolvers(void)
{
  typedef CUresult(CUDAAPI * create_type)(
      CUmemGenericAllocationHandle*, const CUmulticastObjectProp*);
  void* cuda = dlopen("libcuda.so.1", RTLD_NOW);
  void* symbol = NULL;
  CUmemGenericAllocationHandle handle = 0;
  CUmulticastObjectProp props = properties();
  enum cudaDriverEntryPointQueryResult runtime_status;

  require(cuda != NULL, "dlopen libcuda");
  symbol = dlsym(cuda, "cuMulticastCreate");
  require(
      symbol != NULL && symbol != (void*)&fakeCuMulticastCreateOriginal,
      "dlsym substitution");
  require(
      ((create_type)symbol)(&handle, &props) == CUDA_SUCCESS,
      "dlsym behavior");
  require_logical_handle(handle, "dlsym logical handle");

  symbol = NULL;
  require(
      cuGetProcAddress("cuMulticastCreate", &symbol, CUDA_VERSION, 0) == CUDA_SUCCESS,
      "cuGetProcAddress");
  require(
      symbol != NULL && symbol != (void*)&fakeCuMulticastCreateOriginal,
      "cuGetProcAddress substitution");
  handle = 0;
  require(
      ((create_type)symbol)(&handle, &props) == CUDA_SUCCESS,
      "cuGetProcAddress behavior");
  require_logical_handle(handle, "cuGetProcAddress logical handle");

  symbol = NULL;
  require(
      cudaGetDriverEntryPoint(
          "cuMulticastCreate", &symbol, 0, &runtime_status) == cudaSuccess,
      "cudaGetDriverEntryPoint");
  require(
      runtime_status == cudaDriverEntryPointSuccess &&
          symbol != (void*)&fakeCuMulticastCreateOriginal,
      "runtime substitution");
  handle = 0;
  require(
      ((create_type)symbol)(&handle, &props) == CUDA_SUCCESS,
      "runtime behavior");
  require_logical_handle(handle, "runtime logical handle");
  dlclose(cuda);
}

int
main(void)
{
  test_tracked_behavior();
  test_resolvers();
  puts("multicast behavior OK");
  return 0;
}
