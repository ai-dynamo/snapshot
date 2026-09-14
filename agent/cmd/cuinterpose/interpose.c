/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * CUDA virtual memory management (VMM) entry points the shim replaces.
 *
 * This layer forwards every call to the real driver unchanged. It exists so
 * that symbol resolution (symbols.c) can be built and tested on its own; the
 * bookkeeping that tracks shared allocations is added on top of these
 * wrappers in a later change.
 */

#define _GNU_SOURCE

#include <cuda.h>

#include "export.h"
#include "protocol.h"
#include "symbols.h"

CUINTERPOSE_API const struct cuinterpose_build_info cuinterpose_build_info = {
    .cuda_version = CUDA_VERSION,
    .protocol_version = CUINTERPOSE_VERSION,
};

typedef CUresult(CUDAAPI* create_fn)(
    CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);
typedef CUresult(CUDAAPI* release_fn)(CUmemGenericAllocationHandle);
typedef CUresult(CUDAAPI* retain_fn)(CUmemGenericAllocationHandle*, void*);
typedef CUresult(CUDAAPI* map_fn)(CUdeviceptr, size_t, size_t, CUmemGenericAllocationHandle, unsigned long long);
typedef CUresult(CUDAAPI* unmap_fn)(CUdeviceptr, size_t);
typedef CUresult(CUDAAPI* access_fn)(CUdeviceptr, size_t, const CUmemAccessDesc*, size_t);
typedef CUresult(CUDAAPI* export_fn)(
    void*, CUmemGenericAllocationHandle, CUmemAllocationHandleType, unsigned long long);
typedef CUresult(CUDAAPI* import_fn)(CUmemGenericAllocationHandle*, void*, CUmemAllocationHandleType);
typedef CUresult(CUDAAPI* properties_fn)(CUmemAllocationProp*, CUmemGenericAllocationHandle);

CUINTERPOSE_API CUresult CUDAAPI
cuMemCreate(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties, unsigned long long flags)
{
  create_fn function = (create_fn)cuinterpose_lookup_real_symbol("cuMemCreate");

  return function != NULL ? function(output, size, properties, flags) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemRelease(CUmemGenericAllocationHandle handle)
{
  release_fn function = (release_fn)cuinterpose_lookup_real_symbol("cuMemRelease");

  return function != NULL ? function(handle) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemRetainAllocationHandle(CUmemGenericAllocationHandle* output, void* address)
{
  retain_fn function = (retain_fn)cuinterpose_lookup_real_symbol("cuMemRetainAllocationHandle");

  return function != NULL ? function(output, address) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemMap(CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle, unsigned long long flags)
{
  map_fn function = (map_fn)cuinterpose_lookup_real_symbol("cuMemMap");

  return function != NULL ? function(address, size, offset, handle, flags) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemUnmap(CUdeviceptr address, size_t size)
{
  unmap_fn function = (unmap_fn)cuinterpose_lookup_real_symbol("cuMemUnmap");

  return function != NULL ? function(address, size) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  access_fn function = (access_fn)cuinterpose_lookup_real_symbol("cuMemSetAccess");

  return function != NULL ? function(address, size, descriptors, count) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemExportToShareableHandle(
    void* shareable, CUmemGenericAllocationHandle handle, CUmemAllocationHandleType type, unsigned long long flags)
{
  export_fn function = (export_fn)cuinterpose_lookup_real_symbol("cuMemExportToShareableHandle");

  return function != NULL ? function(shareable, handle, type, flags) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemImportFromShareableHandle(CUmemGenericAllocationHandle* output, void* os_handle, CUmemAllocationHandleType type)
{
  import_fn function = (import_fn)cuinterpose_lookup_real_symbol("cuMemImportFromShareableHandle");

  return function != NULL ? function(output, os_handle, type) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp* properties, CUmemGenericAllocationHandle handle)
{
  properties_fn function = (properties_fn)cuinterpose_lookup_real_symbol("cuMemGetAllocationPropertiesFromHandle");

  return function != NULL ? function(properties, handle) : cuinterpose_unavailable();
}
