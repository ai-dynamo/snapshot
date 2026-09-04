/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * How the shim gets between an application and libcuda.
 *
 * Applications reach CUDA driver functions four ways, and the shim must cover
 * all of them or a call slips past untracked:
 *
 *  1. Direct linking: the executable or a library has an undefined reference
 *     to cuMemCreate. LD_PRELOAD puts this library first in the search order,
 *     so the reference binds to the wrapper in interpose.c.
 *  2. dlsym(): code loads libcuda.so.1 by hand and looks symbols up. The shim
 *     replaces dlsym itself; when the looked-up symbol comes from libcuda or
 *     libcudart and the shim has a wrapper for it, the wrapper is returned.
 *  3. cuGetProcAddress(): the CUDA runtime (including the one statically linked
 *     into PyTorch) resolves every driver function through this. The wrapper
 *     asks the real driver first, then substitutes.
 *  4. cudaGetDriverEntryPoint(): the runtime's public version of 3.
 *
 * To call the *real* functions the shim needs a dlsym that is not itself.
 * dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34") fetches glibc's, which is why the
 * shim requires glibc 2.34 or newer and why the Makefile pins that version.
 */

#define _GNU_SOURCE

#include "symbols.h"

#include <cuda_runtime_api.h>
#include <dlfcn.h>
#include <link.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdint.h>
#include <string.h>

#include "export.h"
#include "interpose.h"

#ifndef CUINTERPOSE_DLSYM_VERSION
#error "CUINTERPOSE_DLSYM_VERSION must be set by the build"
#endif

/* cuda.h maps these names to versioned entry points; the shim wraps the names. */
#undef cuGetProcAddress
#undef cuMulticastBindAddr
#undef cuMulticastBindMem
#undef cudaGetDriverEntryPoint
#undef cudaGetDriverEntryPointByVersion

/* Wrappers defined in interpose.c and multicast.c. */
CUresult CUDAAPI cuMemCreate(CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);
CUresult CUDAAPI cuMemRelease(CUmemGenericAllocationHandle);
CUresult CUDAAPI cuMemRetainAllocationHandle(CUmemGenericAllocationHandle*, void*);
CUresult CUDAAPI cuMemMap(CUdeviceptr, size_t, size_t, CUmemGenericAllocationHandle, unsigned long long);
CUresult CUDAAPI cuMemUnmap(CUdeviceptr, size_t);
CUresult CUDAAPI cuMemSetAccess(CUdeviceptr, size_t, const CUmemAccessDesc*, size_t);
CUresult CUDAAPI cuMemExportToShareableHandle(
    void*, CUmemGenericAllocationHandle, CUmemAllocationHandleType, unsigned long long);
CUresult CUDAAPI cuMemImportFromShareableHandle(CUmemGenericAllocationHandle*, void*, CUmemAllocationHandleType);
CUresult CUDAAPI cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp*, CUmemGenericAllocationHandle);
CUresult CUDAAPI cuMulticastCreate(CUmemGenericAllocationHandle*, const CUmulticastObjectProp*);
CUresult CUDAAPI cuMulticastAddDevice(CUmemGenericAllocationHandle, CUdevice);
CUresult CUDAAPI cuMulticastBindMem(
    CUmemGenericAllocationHandle, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
CUresult CUDAAPI cuMulticastBindAddr(CUmemGenericAllocationHandle, size_t, CUdeviceptr, size_t, unsigned long long);
#if CUDA_VERSION >= 13010
CUresult CUDAAPI cuMulticastBindMem_v2(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
CUresult CUDAAPI cuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUdeviceptr, size_t, unsigned long long);
#endif
CUresult CUDAAPI cuMulticastGetGranularity(size_t*, const CUmulticastObjectProp*, CUmulticastGranularity_flags);
CUresult CUDAAPI cuMulticastUnbind(CUmemGenericAllocationHandle, CUdevice, size_t, size_t);

/* Resolver wrappers defined below. */
CUINTERPOSE_API CUresult CUDAAPI cuGetProcAddress(const char*, void**, int, cuuint64_t);
CUINTERPOSE_API CUresult CUDAAPI
cuGetProcAddress_v2(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
CUINTERPOSE_API CUresult CUDAAPI
cuGetProcAddress_v2_ptsz(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
CUINTERPOSE_API cudaError_t CUDARTAPI
cudaGetDriverEntryPoint(const char*, void**, unsigned long long, enum cudaDriverEntryPointQueryResult*);
CUINTERPOSE_API cudaError_t CUDARTAPI cudaGetDriverEntryPointByVersion(
    const char*, void**, unsigned int, unsigned long long, enum cudaDriverEntryPointQueryResult*);
CUINTERPOSE_API cudaError_t CUDARTAPI
cudaGetDriverEntryPoint_ptsz(const char*, void**, unsigned long long, enum cudaDriverEntryPointQueryResult*);
CUINTERPOSE_API cudaError_t CUDARTAPI cudaGetDriverEntryPointByVersion_ptsz(
    const char*, void**, unsigned int, unsigned long long, enum cudaDriverEntryPointQueryResult*);
CUINTERPOSE_API void* dlsym(void*, const char*);

static pthread_once_t real_dlsym_once = PTHREAD_ONCE_INIT;
static void* (*real_dlsym_function)(void*, const char*);
/*
 * Handles the application used to load libcuda/libcudart with dlopen(). When
 * a library is loaded RTLD_LOCAL (the CUDA runtime does this), RTLD_NEXT
 * cannot see its symbols, so the shim remembers the handle it saw in dlsym()
 * and falls back to it.
 */
static _Atomic(uintptr_t) explicit_libcuda_handle;
static _Atomic(uintptr_t) explicit_libcudart_handle;
static _Atomic(uintptr_t) explicit_cu_get_proc_address;
static _Atomic(uintptr_t) explicit_cu_get_proc_address_v2;

static void* replacement(const char*, int);

CUresult
cuinterpose_unavailable(void)
{
  return CUDA_ERROR_NOT_INITIALIZED;
}

static void
initialize_real_dlsym(void)
{
  real_dlsym_function = (void* (*)(void*, const char*))dlvsym(RTLD_NEXT, "dlsym", CUINTERPOSE_DLSYM_VERSION);
}

static void*
real_dlsym(void* handle, const char* name)
{
  if (pthread_once(&real_dlsym_once, initialize_real_dlsym) != 0 || real_dlsym_function == NULL)
    return NULL;
  return real_dlsym_function(handle, name);
}

void*
cuinterpose_lookup_real_symbol(const char* name)
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

/* True when `symbol` was defined by libcuda.so* or libcudart.so*. */
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

CUINTERPOSE_API void*
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
  } else if (handle != NULL && handle != RTLD_NEXT) {
    atomic_store(&explicit_libcudart_handle, (uintptr_t)handle);
  }
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

  /* The CUDA runtime resolves driver functions before any VMM call, so this is
   * the earliest point at which a fork child can announce itself. */
  (void)cuinterpose_ensure_process_endpoint();
  if (strcmp(resolver, "cudaGetDriverEntryPoint") == 0 || strcmp(resolver, "cudaGetDriverEntryPoint_ptsz") == 0) {
    typedef cudaError_t(CUDARTAPI * function_type)(
        const char*, void**, unsigned long long, enum cudaDriverEntryPointQueryResult*);
    function_type function = (function_type)cuinterpose_lookup_real_symbol(resolver);
    result = function != NULL ? function(symbol, output, flags, status) : cudaErrorInitializationError;
  } else {
    typedef cudaError_t(CUDARTAPI * function_type)(
        const char*, void**, unsigned int, unsigned long long, enum cudaDriverEntryPointQueryResult*);
    function_type function = (function_type)cuinterpose_lookup_real_symbol(resolver);
    result = function != NULL ? function(symbol, output, version, flags, status) : cudaErrorInitializationError;
  }
  /* Only substitute after the real resolver succeeded: the shim never invents entry points. */
  if (result == cudaSuccess && output != NULL && *output != NULL &&
      (status == NULL || *status == cudaDriverEntryPointSuccess) &&
      (entry = replacement(symbol, version == 0 ? CUDA_VERSION : (int)version)) != NULL)
    *output = entry;
  return result;
}

CUINTERPOSE_API cudaError_t CUDARTAPI
cudaGetDriverEntryPoint(
    const char* symbol, void** output, unsigned long long flags, enum cudaDriverEntryPointQueryResult* status)
{
  return runtime_driver_entry_point("cudaGetDriverEntryPoint", symbol, output, 0, flags, status);
}

CUINTERPOSE_API cudaError_t CUDARTAPI
cudaGetDriverEntryPoint_ptsz(
    const char* symbol, void** output, unsigned long long flags, enum cudaDriverEntryPointQueryResult* status)
{
  return runtime_driver_entry_point("cudaGetDriverEntryPoint_ptsz", symbol, output, 0, flags, status);
}

CUINTERPOSE_API cudaError_t CUDARTAPI
cudaGetDriverEntryPointByVersion(
    const char* symbol, void** output, unsigned int version, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  return runtime_driver_entry_point("cudaGetDriverEntryPointByVersion", symbol, output, version, flags, status);
}

CUINTERPOSE_API cudaError_t CUDARTAPI
cudaGetDriverEntryPointByVersion_ptsz(
    const char* symbol, void** output, unsigned int version, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  return runtime_driver_entry_point("cudaGetDriverEntryPointByVersion_ptsz", symbol, output, version, flags, status);
}

/*
 * The replacement table. `version` is the CUDA version the caller asked the
 * resolver for; it selects the ABI when a function has more than one.
 */
static void*
replacement(const char* symbol, int version)
{
#define ENTRY(name)               \
  if (strcmp(symbol, #name) == 0) \
  return (void*)&name
  if (symbol == NULL)
    return NULL;
#if CUDA_VERSION >= 13010
  /* CUDA 13.1 added device-explicit bind entry points; a 13.1+ caller gets them. */
  if (version >= 13010 && strcmp(symbol, "cuMulticastBindMem") == 0)
    return (void*)&cuMulticastBindMem_v2;
  if (version >= 13010 && strcmp(symbol, "cuMulticastBindAddr") == 0)
    return (void*)&cuMulticastBindAddr_v2;
#endif
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
#if CUDA_VERSION >= 13010
  ENTRY(cuMulticastBindMem_v2);
#endif
  ENTRY(cuMulticastBindAddr);
#if CUDA_VERSION >= 13010
  ENTRY(cuMulticastBindAddr_v2);
#endif
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

CUINTERPOSE_API CUresult CUDAAPI
cuGetProcAddress(const char* symbol, void** output, int version, cuuint64_t flags)
{
  typedef CUresult(CUDAAPI * function_type)(const char*, void**, int, cuuint64_t);
  function_type function = (function_type)atomic_load(&explicit_cu_get_proc_address);
  CUresult result;
  void* entry;

  (void)cuinterpose_ensure_process_endpoint();
  if (function == NULL)
    function = (function_type)cuinterpose_lookup_real_symbol("cuGetProcAddress");
  result = function != NULL ? function(symbol, output, version, flags) : cuinterpose_unavailable();
  if (result == CUDA_SUCCESS && output != NULL && *output != NULL && (entry = replacement(symbol, version)) != NULL)
    *output = entry;
  return result;
}

CUINTERPOSE_API CUresult CUDAAPI
cuGetProcAddress_v2(
    const char* symbol, void** output, int version, cuuint64_t flags, CUdriverProcAddressQueryResult* status)
{
  typedef CUresult(CUDAAPI * function_type)(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
  function_type function = (function_type)atomic_load(&explicit_cu_get_proc_address_v2);
  CUresult result;
  void* entry;

  (void)cuinterpose_ensure_process_endpoint();
  if (function == NULL)
    function = (function_type)cuinterpose_lookup_real_symbol("cuGetProcAddress_v2");
  result = function != NULL ? function(symbol, output, version, flags, status) : cuinterpose_unavailable();
  if (result == CUDA_SUCCESS && output != NULL && *output != NULL &&
      (status == NULL || *status == CU_GET_PROC_ADDRESS_SUCCESS) && (entry = replacement(symbol, version)) != NULL)
    *output = entry;
  return result;
}

CUINTERPOSE_API CUresult CUDAAPI
cuGetProcAddress_v2_ptsz(
    const char* symbol, void** output, int version, cuuint64_t flags, CUdriverProcAddressQueryResult* status)
{
  typedef CUresult(CUDAAPI * function_type)(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
  function_type function = (function_type)atomic_load(&explicit_cu_get_proc_address_v2);
  const cuuint64_t stream_flags = CU_GET_PROC_ADDRESS_LEGACY_STREAM | CU_GET_PROC_ADDRESS_PER_THREAD_DEFAULT_STREAM;
  CUresult result;
  void* entry;

  (void)cuinterpose_ensure_process_endpoint();
  if (function == NULL)
    function = (function_type)cuinterpose_lookup_real_symbol("cuGetProcAddress_v2");
  if ((flags & stream_flags) == 0)
    flags |= CU_GET_PROC_ADDRESS_PER_THREAD_DEFAULT_STREAM;
  result = function != NULL ? function(symbol, output, version, flags, status) : cuinterpose_unavailable();
  if (result == CUDA_SUCCESS && output != NULL && *output != NULL &&
      (status == NULL || *status == CU_GET_PROC_ADDRESS_SUCCESS) && (entry = replacement(symbol, version)) != NULL)
    *output = entry;
  return result;
}
