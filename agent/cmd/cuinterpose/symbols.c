/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "symbols.h"

#include <cuda_runtime_api.h>
#include <dlfcn.h>
#include <link.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

#undef cuGetProcAddress
#undef cudaGetDriverEntryPoint
#undef cudaGetDriverEntryPointByVersion

CUresult CUDAAPI cuMemCreate(CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);
CUresult CUDAAPI cuMemRelease(CUmemGenericAllocationHandle);
CUresult CUDAAPI cuMemRetainAllocationHandle(CUmemGenericAllocationHandle*, void*);
CUresult CUDAAPI cuMemMap(CUdeviceptr, size_t, size_t, CUmemGenericAllocationHandle, unsigned long long);
CUresult CUDAAPI cuMemUnmap(CUdeviceptr, size_t);
CUresult CUDAAPI cuMemSetAccess(CUdeviceptr, size_t, const CUmemAccessDesc*, size_t);
CUresult CUDAAPI cuMemExportToShareableHandle(void*, CUmemGenericAllocationHandle, CUmemAllocationHandleType, unsigned long long);
CUresult CUDAAPI cuMemImportFromShareableHandle(CUmemGenericAllocationHandle*, void*, CUmemAllocationHandleType);
CUresult CUDAAPI cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp*, CUmemGenericAllocationHandle);
CUresult CUDAAPI cuMulticastCreate(CUmemGenericAllocationHandle*, const CUmulticastObjectProp*);
CUresult CUDAAPI cuMulticastAddDevice(CUmemGenericAllocationHandle, CUdevice);
CUresult CUDAAPI cuMulticastBindMem(CUmemGenericAllocationHandle, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
CUresult CUDAAPI cuMulticastBindMem_v2(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
CUresult CUDAAPI cuMulticastBindAddr(CUmemGenericAllocationHandle, size_t, CUdeviceptr, size_t, unsigned long long);
CUresult CUDAAPI cuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUdeviceptr, size_t, unsigned long long);
CUresult CUDAAPI cuMulticastGetGranularity(size_t*, const CUmulticastObjectProp*, CUmulticastGranularity_flags);
CUresult CUDAAPI cuMulticastUnbind(CUmemGenericAllocationHandle, CUdevice, size_t, size_t);
CUresult CUDAAPI cuGetProcAddress(const char*, void**, int, cuuint64_t);
CUresult CUDAAPI cuGetProcAddress_v2(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
CUresult CUDAAPI cuGetProcAddress_v2_ptsz(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
cudaError_t CUDARTAPI cudaGetDriverEntryPoint(const char*, void**, unsigned long long, enum cudaDriverEntryPointQueryResult*);
cudaError_t CUDARTAPI cudaGetDriverEntryPointByVersion(
    const char*, void**, unsigned int, unsigned long long, enum cudaDriverEntryPointQueryResult*);
cudaError_t CUDARTAPI cudaGetDriverEntryPoint_ptsz(const char*, void**, unsigned long long, enum cudaDriverEntryPointQueryResult*);
cudaError_t CUDARTAPI cudaGetDriverEntryPointByVersion_ptsz(
    const char*, void**, unsigned int, unsigned long long, enum cudaDriverEntryPointQueryResult*);

static pthread_once_t real_dlsym_once = PTHREAD_ONCE_INIT;
static void* (*real_dlsym_function)(void*, const char*);
static _Atomic(uintptr_t) explicit_libcuda_handle;
static _Atomic(uintptr_t) explicit_libcudart_handle;
static _Atomic(uintptr_t) explicit_cu_get_proc_address;
static _Atomic(uintptr_t) explicit_cu_get_proc_address_v2;

static void* replacement(const char*, int);

CUresult
unavailable(void)
{
  return CUDA_ERROR_NOT_INITIALIZED;
}

static void
initialize_real_dlsym(void)
{
  /*
   * A dlsym interposer cannot call dlsym(RTLD_NEXT, ...) without recursing,
   * and POSIX provides no alternate next-definition lookup. dlvsym is the
   * simplest public glibc interface; the symbol version varies by architecture.
   */
  static const char* versions[] = {"GLIBC_2.2.5", "GLIBC_2.17", "GLIBC_2.34"};
  size_t index;

  for (index = 0; index < sizeof(versions) / sizeof(versions[0]); index++) {
    real_dlsym_function = (void* (*)(void*, const char*))dlvsym(RTLD_NEXT, "dlsym", versions[index]);
    if (real_dlsym_function != NULL)
      return;
  }
}

static void*
real_dlsym(void* handle, const char* name)
{
  if (pthread_once(&real_dlsym_once, initialize_real_dlsym) != 0 || real_dlsym_function == NULL)
    return NULL;
  return real_dlsym_function(handle, name);
}

void*
lookup_real_symbol(const char* name)
{
  void* symbol = real_dlsym(RTLD_NEXT, name);
  void* handle;

  if (symbol != NULL)
    return symbol;
  handle = (void*)atomic_load(&explicit_libcuda_handle);
  if (handle != NULL && (symbol = real_dlsym(handle, name)) != NULL)
    return symbol;
  handle = (void*)atomic_load(&explicit_libcudart_handle);
  return handle == NULL ? NULL : real_dlsym(handle, name);
}

static bool
is_cuda_library(void* handle, void* symbol, const char** library)
{
  struct link_map* map;
  Dl_info info;
  const char* provider;
  const char* requested;

  if (dladdr(symbol, &info) == 0)
    return false;
  provider = strrchr(info.dli_fname, '/');
  provider = provider == NULL ? info.dli_fname : provider + 1;
  if (strncmp(provider, "libcuda.so", 10) != 0 && strncmp(provider, "libcudart.so", 12) != 0)
    return false;
  *library = provider;
  if (handle == NULL || handle == RTLD_NEXT)
    return true;
  if (dlinfo(handle, RTLD_DI_LINKMAP, &map) != 0 || map == NULL)
    return false;
  requested = strrchr(map->l_name, '/');
  requested = requested == NULL ? map->l_name : requested + 1;
  return (strncmp(requested, "libcuda.so", 10) == 0 && strncmp(provider, "libcuda.so", 10) == 0) ||
         (strncmp(requested, "libcudart.so", 12) == 0 && strncmp(provider, "libcudart.so", 12) == 0);
}

void*
dlsym(void* handle, const char* name)
{
  void* symbol = real_dlsym(handle, name);
  void* entry;
  const char* library;

  if (symbol == NULL || !is_cuda_library(handle, symbol, &library))
    return symbol;
  if (strncmp(library, "libcuda.so", 10) == 0) {
    if (handle != NULL && handle != RTLD_NEXT)
      atomic_store(&explicit_libcuda_handle, (uintptr_t)handle);
    if (strcmp(name, "cuGetProcAddress") == 0)
      atomic_store(&explicit_cu_get_proc_address, (uintptr_t)symbol);
    if (strcmp(name, "cuGetProcAddress_v2") == 0)
      atomic_store(&explicit_cu_get_proc_address_v2, (uintptr_t)symbol);
  } else if (handle != NULL && handle != RTLD_NEXT)
    atomic_store(&explicit_libcudart_handle, (uintptr_t)handle);
  entry = replacement(name, 0);
  return entry == NULL || entry == symbol ? symbol : entry;
}

static cudaError_t
runtime_driver_entry_point(
    const char* resolver, const char* symbol, void** output, unsigned int version, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  cudaError_t result;
  void* entry;

  if (strcmp(resolver, "cudaGetDriverEntryPoint") == 0 || strcmp(resolver, "cudaGetDriverEntryPoint_ptsz") == 0) {
    typedef cudaError_t(CUDARTAPI * legacy_type)(
        const char*, void**, unsigned long long, enum cudaDriverEntryPointQueryResult*);
    legacy_type legacy = (legacy_type)lookup_real_symbol(resolver);
    result = legacy != NULL ? legacy(symbol, output, flags, status) : cudaErrorInitializationError;
  } else {
    typedef cudaError_t(CUDARTAPI * function_type)(
        const char*, void**, unsigned int, unsigned long long, enum cudaDriverEntryPointQueryResult*);
    function_type function = (function_type)lookup_real_symbol(resolver);
    result = function != NULL ? function(symbol, output, version, flags, status) : cudaErrorInitializationError;
  }
  if (result == cudaSuccess && output != NULL && *output != NULL &&
      (status == NULL || *status == cudaDriverEntryPointSuccess) &&
      (entry = replacement(symbol, version == 0 ? CUDA_VERSION : (int)version)) != NULL)
    *output = entry;
  return result;
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPoint(
    const char* symbol, void** output, unsigned long long flags, enum cudaDriverEntryPointQueryResult* status)
{
  return runtime_driver_entry_point("cudaGetDriverEntryPoint", symbol, output, 0, flags, status);
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPoint_ptsz(
    const char* symbol, void** output, unsigned long long flags, enum cudaDriverEntryPointQueryResult* status)
{
  return runtime_driver_entry_point("cudaGetDriverEntryPoint_ptsz", symbol, output, 0, flags, status);
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPointByVersion(
    const char* symbol, void** output, unsigned int version, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  return runtime_driver_entry_point("cudaGetDriverEntryPointByVersion", symbol, output, version, flags, status);
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPointByVersion_ptsz(
    const char* symbol, void** output, unsigned int version, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  return runtime_driver_entry_point("cudaGetDriverEntryPointByVersion_ptsz", symbol, output, version, flags, status);
}

static void*
replacement(const char* symbol, int version)
{
#define ENTRY(name)               \
  if (strcmp(symbol, #name) == 0) \
  return (void*)&name
  if (symbol == NULL)
    return NULL;
  ENTRY(cuMemCreate);
  ENTRY(cuMemRelease);
  ENTRY(cuMemRetainAllocationHandle);
  ENTRY(cuMemMap);
  ENTRY(cuMemUnmap);
  ENTRY(cuMemSetAccess);
  ENTRY(cuMemExportToShareableHandle);
  ENTRY(cuMemImportFromShareableHandle);
  ENTRY(cuMemGetAllocationPropertiesFromHandle);
  ENTRY(cuMulticastCreate);
  ENTRY(cuMulticastAddDevice);
  ENTRY(cuMulticastBindMem);
  ENTRY(cuMulticastBindMem_v2);
  ENTRY(cuMulticastBindAddr);
  ENTRY(cuMulticastBindAddr_v2);
  ENTRY(cuMulticastGetGranularity);
  ENTRY(cuMulticastUnbind);
  ENTRY(cuGetProcAddress_v2);
  ENTRY(cuGetProcAddress_v2_ptsz);
  ENTRY(cudaGetDriverEntryPoint);
  ENTRY(cudaGetDriverEntryPoint_ptsz);
  ENTRY(cudaGetDriverEntryPointByVersion);
  ENTRY(cudaGetDriverEntryPointByVersion_ptsz);
#undef ENTRY
  if (strcmp(symbol, "cuGetProcAddress") == 0)
    return version >= 12000 ? (void*)&cuGetProcAddress_v2 : (void*)&cuGetProcAddress;
  return NULL;
}

CUresult CUDAAPI
cuGetProcAddress(const char* symbol, void** output, int version, cuuint64_t flags)
{
  typedef CUresult(CUDAAPI * function_type)(const char*, void**, int, cuuint64_t);
  function_type function = (function_type)atomic_load(&explicit_cu_get_proc_address);
  CUresult result;
  void* entry;

  if (function == NULL)
    function = (function_type)lookup_real_symbol("cuGetProcAddress");
  result = function != NULL ? function(symbol, output, version, flags) : unavailable();
  if (result == CUDA_SUCCESS && output != NULL && *output != NULL &&
      (entry = replacement(symbol, version)) != NULL)
    *output = entry;
  return result;
}

CUresult CUDAAPI
cuGetProcAddress_v2(
    const char* symbol, void** output, int version, cuuint64_t flags, CUdriverProcAddressQueryResult* status)
{
  typedef CUresult(CUDAAPI * function_type)(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
  function_type function = (function_type)atomic_load(&explicit_cu_get_proc_address_v2);
  CUresult result;
  void* entry;

  if (function == NULL)
    function = (function_type)lookup_real_symbol("cuGetProcAddress_v2");
  result = function != NULL ? function(symbol, output, version, flags, status) : unavailable();
  if (result == CUDA_SUCCESS && output != NULL && *output != NULL &&
      (status == NULL || *status == CU_GET_PROC_ADDRESS_SUCCESS) && (entry = replacement(symbol, version)) != NULL)
    *output = entry;
  return result;
}

CUresult CUDAAPI
cuGetProcAddress_v2_ptsz(
    const char* symbol, void** output, int version, cuuint64_t flags, CUdriverProcAddressQueryResult* status)
{
  typedef CUresult(CUDAAPI * function_type)(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
  function_type function = (function_type)atomic_load(&explicit_cu_get_proc_address_v2);
  cuuint64_t stream_flags = CU_GET_PROC_ADDRESS_LEGACY_STREAM | CU_GET_PROC_ADDRESS_PER_THREAD_DEFAULT_STREAM;
  CUresult result;
  void* entry;

  if (function == NULL)
    function = (function_type)lookup_real_symbol("cuGetProcAddress_v2");
  if ((flags & stream_flags) == 0)
    flags |= CU_GET_PROC_ADDRESS_PER_THREAD_DEFAULT_STREAM;
  result = function != NULL ? function(symbol, output, version, flags, status) : unavailable();
  if (result == CUDA_SUCCESS && output != NULL && *output != NULL &&
      (status == NULL || *status == CU_GET_PROC_ADDRESS_SUCCESS) && (entry = replacement(symbol, version)) != NULL)
    *output = entry;
  return result;
}
