/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include <cuda.h>
#include <cuda_runtime_api.h>
#include <dlfcn.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

#include "../protocol.h"

#undef cuGetProcAddress
#undef cuMulticastBindAddr
#undef cuMulticastBindMem

CUresult CUDAAPI cuGetProcAddress(const char*, void**, int, cuuint64_t);

extern CUresult CUDAAPI fakeCuMulticastCreateOriginal(
    CUmemGenericAllocationHandle*, const CUmulticastObjectProp*);
extern void fakeEnableBlockingMulticastMap(void);
extern int fakeMulticastMapEntered(void);
extern void fakeReleaseMulticastMap(void);

struct map_call {
  CUmemGenericAllocationHandle handle;
  CUresult result;
};

struct add_device_call {
  CUmemGenericAllocationHandle handle;
  CUresult result;
  int done;
};

static CUmulticastObjectProp properties(void);

static void
require(int condition, const char* message)
{
  if (!condition) {
    fprintf(stderr, "FAIL: %s\n", message);
    exit(1);
  }
}

static void
test_multicast_records_driver_accepted_extent(void)
{
  typedef size_t (*record_count_fn)(void);
  typedef int (*write_records_fn)(struct cuinterposer_record*, size_t);
  record_count_fn record_count = (record_count_fn)dlsym(RTLD_DEFAULT, "cuinterposer_multicast_record_count");
  write_records_fn write_records =
      (write_records_fn)dlsym(RTLD_DEFAULT, "cuinterposer_multicast_write_records");
  CUmulticastObjectProp props = properties();
  struct cuinterposer_record* records;
  CUmemGenericAllocationHandle handle;
  size_t count;
  size_t index;
  int found = 0;

  props.size = 2048;
  require(record_count != NULL && write_records != NULL, "multicast record API");
  require(cuMulticastCreate(&handle, &props) == CUDA_SUCCESS, "create multicast with sub-extent request");
  require(cuMemMap(0x3000, 8192, 0, handle, 0) == CUDA_SUCCESS, "driver accepts larger multicast mapping");
  count = record_count();
  records = calloc(count, sizeof(*records));
  require(records != NULL, "allocate multicast records");
  require(write_records(records, count) == 0, "write multicast records");
  for (index = 0; index < count; index++) {
    if (records[index].kind == CUINTERPOSER_MULTICAST && records[index].allocation_size == 8192)
      found = 1;
  }
  free(records);
  require(found, "multicast record uses driver-accepted extent");
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

static void*
map_multicast(void* argument)
{
  struct map_call* call = argument;

  call->result = cuMemMap(0x2000, 4096, 0, call->handle, 0);
  return NULL;
}

static void*
add_multicast_device(void* argument)
{
  struct add_device_call* call = argument;

  call->result = cuMulticastAddDevice(call->handle, 1);
  __atomic_store_n(&call->done, 1, __ATOMIC_RELEASE);
  return NULL;
}

static void
test_multicast_map_releases_state_lock(void)
{
  struct map_call map_call = {0};
  struct add_device_call add_call = {0};
  CUmulticastObjectProp props = properties();
  pthread_t map_thread;
  pthread_t add_thread;
  int completed_while_blocked;
  int i;

  require(cuMulticastCreate(&map_call.handle, &props) == CUDA_SUCCESS, "cuMulticastCreate for map");
  add_call.handle = map_call.handle;
  fakeEnableBlockingMulticastMap();
  require(pthread_create(&map_thread, NULL, map_multicast, &map_call) == 0, "start multicast map");
  for (i = 0; i < 1000 && !fakeMulticastMapEntered(); i++)
    usleep(1000);
  require(fakeMulticastMapEntered(), "real multicast map entered");

  require(pthread_create(&add_thread, NULL, add_multicast_device, &add_call) == 0, "start peer state call");
  for (i = 0; i < 1000 && !__atomic_load_n(&add_call.done, __ATOMIC_ACQUIRE); i++)
    usleep(1000);
  completed_while_blocked = __atomic_load_n(&add_call.done, __ATOMIC_ACQUIRE);
  fakeReleaseMulticastMap();
  require(pthread_join(add_thread, NULL) == 0, "join peer state call");
  require(pthread_join(map_thread, NULL) == 0, "join multicast map");

  require(completed_while_blocked, "multicast map released interposer state lock");
  require(add_call.result == (CUresult)111, "peer state call result");
  require(map_call.result == CUDA_SUCCESS, "multicast map result");
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
  test_multicast_map_releases_state_lock();
  test_resolvers();
  test_multicast_records_driver_accepted_extent();
  puts("multicast behavior OK");
  return 0;
}
