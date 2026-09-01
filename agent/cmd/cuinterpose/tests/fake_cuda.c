/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include <cuda.h>
#include <dlfcn.h>
#include <string.h>

#undef cuGetProcAddress
#undef cuMulticastBindAddr
#undef cuMulticastBindMem

enum {
  result_create = 101,
  result_release,
  result_retain,
  result_map,
  result_unmap,
  result_access,
  result_export,
  result_import,
  result_properties,
  result_multicast_create,
  result_multicast_add_device,
  result_multicast_bind_mem,
  result_multicast_bind_address,
  result_multicast_granularity,
  result_multicast_unbind,
};

CUresult CUDAAPI
fakeCuMemCreateOriginal(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties,
    unsigned long long flags)
{
  (void)size;
  (void)properties;
  (void)flags;
  *output = 0xabc;
  return (CUresult)result_create;
}

CUresult CUDAAPI
cuMemCreate(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties,
    unsigned long long flags)
{
  return fakeCuMemCreateOriginal(output, size, properties, flags);
}

CUresult CUDAAPI
cuMemRelease(CUmemGenericAllocationHandle handle)
{
  (void)handle;
  return (CUresult)result_release;
}

CUresult CUDAAPI
cuMemRetainAllocationHandle(CUmemGenericAllocationHandle* output, void* address)
{
  (void)address;
  *output = 0xdef;
  return (CUresult)result_retain;
}

CUresult CUDAAPI
cuMemMap(
    CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle,
    unsigned long long flags)
{
  (void)address;
  (void)size;
  (void)offset;
  (void)handle;
  (void)flags;
  return (CUresult)result_map;
}

CUresult CUDAAPI
cuMemUnmap(CUdeviceptr address, size_t size)
{
  (void)address;
  (void)size;
  return (CUresult)result_unmap;
}

CUresult CUDAAPI
cuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  (void)address;
  (void)size;
  (void)descriptors;
  (void)count;
  return (CUresult)result_access;
}

CUresult CUDAAPI
cuMemExportToShareableHandle(
    void* output, CUmemGenericAllocationHandle handle, CUmemAllocationHandleType type,
    unsigned long long flags)
{
  (void)output;
  (void)handle;
  (void)type;
  (void)flags;
  return (CUresult)result_export;
}

CUresult CUDAAPI
cuMemImportFromShareableHandle(
    CUmemGenericAllocationHandle* output, void* os_handle, CUmemAllocationHandleType type)
{
  (void)os_handle;
  (void)type;
  *output = 0x123;
  return (CUresult)result_import;
}

CUresult CUDAAPI
cuMemGetAllocationPropertiesFromHandle(
    CUmemAllocationProp* properties, CUmemGenericAllocationHandle handle)
{
  (void)properties;
  (void)handle;
  return (CUresult)result_properties;
}

CUresult CUDAAPI
fakeCuMulticastCreateOriginal(
    CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  (void)properties;
  *output = 0x456;
  return (CUresult)result_multicast_create;
}

CUresult CUDAAPI
cuMulticastCreate(
    CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  return fakeCuMulticastCreateOriginal(output, properties);
}

CUresult CUDAAPI
cuMulticastAddDevice(CUmemGenericAllocationHandle multicast, CUdevice device)
{
  (void)multicast;
  (void)device;
  return (CUresult)result_multicast_add_device;
}

CUresult CUDAAPI
cuMulticastBindMem(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size,
    unsigned long long flags)
{
  (void)multicast;
  (void)multicast_offset;
  (void)memory;
  (void)memory_offset;
  (void)size;
  (void)flags;
  return (CUresult)result_multicast_bind_mem;
}

CUresult CUDAAPI
cuMulticastBindAddr(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUdeviceptr memory,
    size_t size, unsigned long long flags)
{
  (void)multicast;
  (void)multicast_offset;
  (void)memory;
  (void)size;
  (void)flags;
  return (CUresult)result_multicast_bind_address;
}

CUresult CUDAAPI
cuMulticastGetGranularity(
    size_t* granularity, const CUmulticastObjectProp* properties,
    CUmulticastGranularity_flags option)
{
  (void)properties;
  (void)option;
  *granularity = 4096;
  return (CUresult)result_multicast_granularity;
}

CUresult CUDAAPI
cuMulticastUnbind(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t offset, size_t size)
{
  (void)multicast;
  (void)device;
  (void)offset;
  (void)size;
  return (CUresult)result_multicast_unbind;
}

static void*
original(const char* symbol)
{
  if (strcmp(symbol, "cuMemCreate") == 0)
    return (void*)&fakeCuMemCreateOriginal;
  if (strcmp(symbol, "cuMulticastCreate") == 0)
    return (void*)&fakeCuMulticastCreateOriginal;
  return dlsym(RTLD_DEFAULT, symbol);
}

CUresult CUDAAPI
cuGetProcAddress(const char* symbol, void** output, int version, cuuint64_t flags)
{
  (void)version;
  (void)flags;
  *output = original(symbol);
  return *output == NULL ? CUDA_ERROR_NOT_FOUND : CUDA_SUCCESS;
}

CUresult CUDAAPI
cuGetProcAddress_v2(
    const char* symbol, void** output, int version, cuuint64_t flags,
    CUdriverProcAddressQueryResult* status)
{
  CUresult result = cuGetProcAddress(symbol, output, version, flags);
  if (status != NULL)
    *status = result == CUDA_SUCCESS ? CU_GET_PROC_ADDRESS_SUCCESS : CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND;
  return result;
}
