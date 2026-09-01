/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "symbols.h"

#define LOOKUP(name, type) ((type)cuinterposer_lookup_real_symbol(#name))

CUresult CUDAAPI
cuMemCreate(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties,
    unsigned long long flags)
{
  typedef CUresult(CUDAAPI * function_type)(
      CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);
  function_type function = LOOKUP(cuMemCreate, function_type);
  return function != NULL ? function(output, size, properties, flags) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMemRelease(CUmemGenericAllocationHandle handle)
{
  typedef CUresult(CUDAAPI * function_type)(CUmemGenericAllocationHandle);
  function_type function = LOOKUP(cuMemRelease, function_type);
  return function != NULL ? function(handle) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMemRetainAllocationHandle(CUmemGenericAllocationHandle* output, void* address)
{
  typedef CUresult(CUDAAPI * function_type)(CUmemGenericAllocationHandle*, void*);
  function_type function = LOOKUP(cuMemRetainAllocationHandle, function_type);
  return function != NULL ? function(output, address) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMemMap(
    CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle, unsigned long long flags)
{
  typedef CUresult(CUDAAPI * function_type)(
      CUdeviceptr, size_t, size_t, CUmemGenericAllocationHandle, unsigned long long);
  function_type function = LOOKUP(cuMemMap, function_type);
  return function != NULL ? function(address, size, offset, handle, flags) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMemUnmap(CUdeviceptr address, size_t size)
{
  typedef CUresult(CUDAAPI * function_type)(CUdeviceptr, size_t);
  function_type function = LOOKUP(cuMemUnmap, function_type);
  return function != NULL ? function(address, size) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  typedef CUresult(CUDAAPI * function_type)(CUdeviceptr, size_t, const CUmemAccessDesc*, size_t);
  function_type function = LOOKUP(cuMemSetAccess, function_type);
  return function != NULL ? function(address, size, descriptors, count) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMemExportToShareableHandle(
    void* output, CUmemGenericAllocationHandle handle, CUmemAllocationHandleType type, unsigned long long flags)
{
  typedef CUresult(CUDAAPI * function_type)(
      void*, CUmemGenericAllocationHandle, CUmemAllocationHandleType, unsigned long long);
  function_type function = LOOKUP(cuMemExportToShareableHandle, function_type);
  return function != NULL ? function(output, handle, type, flags) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMemImportFromShareableHandle(CUmemGenericAllocationHandle* output, void* os_handle, CUmemAllocationHandleType type)
{
  typedef CUresult(CUDAAPI * function_type)(CUmemGenericAllocationHandle*, void*, CUmemAllocationHandleType);
  function_type function = LOOKUP(cuMemImportFromShareableHandle, function_type);
  return function != NULL ? function(output, os_handle, type) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp* properties, CUmemGenericAllocationHandle handle)
{
  typedef CUresult(CUDAAPI * function_type)(CUmemAllocationProp*, CUmemGenericAllocationHandle);
  function_type function = LOOKUP(cuMemGetAllocationPropertiesFromHandle, function_type);
  return function != NULL ? function(properties, handle) : cuinterposer_unavailable();
}

#undef LOOKUP
