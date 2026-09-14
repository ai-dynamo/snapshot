/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "fake_cuda.h"

#include <string.h>

#undef cuGetProcAddress
#undef cuMulticastBindAddr
#undef cuMulticastBindMem

CUresult CUDAAPI fakeCuGetProcAddress(const char*, void**, int, cuuint64_t);

static struct fake_last_call last;

void
fakeReset(void)
{
  memset(&last, 0, sizeof(last));
}

const struct fake_last_call*
fakeLastCall(void)
{
  return &last;
}

int
fakeDriverVersion(void)
{
  return CUDA_VERSION;
}

static void
record(const char* function)
{
  memset(&last, 0, sizeof(last));
  last.function = function;
}

/*
 * Each entry point exists twice: the exported CUDA name, and a fakeXxx alias
 * that cuGetProcAddress hands out. The alias lets a test tell "the shim's
 * wrapper" apart from "the driver's own function".
 */

CUresult CUDAAPI
fakeCuMemCreate(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties, unsigned long long flags)
{
  record("cuMemCreate");
  last.size = size;
  last.flags = flags;
  last.handle_type = properties != NULL ? (int)properties->requestedHandleTypes : -1;
  if (output != NULL)
    *output = 0xabc;
  return (CUresult)FAKE_RESULT_CREATE;
}
CUresult CUDAAPI
cuMemCreate(
    CUmemGenericAllocationHandle* output, size_t size, const CUmemAllocationProp* properties, unsigned long long flags)
{
  return fakeCuMemCreate(output, size, properties, flags);
}

CUresult CUDAAPI
fakeCuMemRelease(CUmemGenericAllocationHandle handle)
{
  record("cuMemRelease");
  last.handle = handle;
  return (CUresult)FAKE_RESULT_RELEASE;
}
CUresult CUDAAPI
cuMemRelease(CUmemGenericAllocationHandle handle)
{
  return fakeCuMemRelease(handle);
}

CUresult CUDAAPI
fakeCuMemRetainAllocationHandle(CUmemGenericAllocationHandle* output, void* address)
{
  record("cuMemRetainAllocationHandle");
  last.pointer = address;
  if (output != NULL)
    *output = 0xdef;
  return (CUresult)FAKE_RESULT_RETAIN;
}
CUresult CUDAAPI
cuMemRetainAllocationHandle(CUmemGenericAllocationHandle* output, void* address)
{
  return fakeCuMemRetainAllocationHandle(output, address);
}

CUresult CUDAAPI
fakeCuMemMap(
    CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle, unsigned long long flags)
{
  record("cuMemMap");
  last.address = address;
  last.size = size;
  last.offset = offset;
  last.handle = handle;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MAP;
}
CUresult CUDAAPI
cuMemMap(CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle, unsigned long long flags)
{
  return fakeCuMemMap(address, size, offset, handle, flags);
}

CUresult CUDAAPI
fakeCuMemUnmap(CUdeviceptr address, size_t size)
{
  record("cuMemUnmap");
  last.address = address;
  last.size = size;
  return (CUresult)FAKE_RESULT_UNMAP;
}
CUresult CUDAAPI
cuMemUnmap(CUdeviceptr address, size_t size)
{
  return fakeCuMemUnmap(address, size);
}

CUresult CUDAAPI
fakeCuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  record("cuMemSetAccess");
  last.address = address;
  last.size = size;
  last.count = count;
  last.pointer = (void*)descriptors;
  return (CUresult)FAKE_RESULT_ACCESS;
}
CUresult CUDAAPI
cuMemSetAccess(CUdeviceptr address, size_t size, const CUmemAccessDesc* descriptors, size_t count)
{
  return fakeCuMemSetAccess(address, size, descriptors, count);
}

CUresult CUDAAPI
fakeCuMemExportToShareableHandle(
    void* output, CUmemGenericAllocationHandle handle, CUmemAllocationHandleType type, unsigned long long flags)
{
  record("cuMemExportToShareableHandle");
  last.pointer = output;
  last.handle = handle;
  last.handle_type = (int)type;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_EXPORT;
}
CUresult CUDAAPI
cuMemExportToShareableHandle(
    void* output, CUmemGenericAllocationHandle handle, CUmemAllocationHandleType type, unsigned long long flags)
{
  return fakeCuMemExportToShareableHandle(output, handle, type, flags);
}

CUresult CUDAAPI
fakeCuMemImportFromShareableHandle(
    CUmemGenericAllocationHandle* output, void* os_handle, CUmemAllocationHandleType type)
{
  record("cuMemImportFromShareableHandle");
  last.pointer = os_handle;
  last.handle_type = (int)type;
  if (output != NULL)
    *output = 0x123;
  return (CUresult)FAKE_RESULT_IMPORT;
}
CUresult CUDAAPI
cuMemImportFromShareableHandle(CUmemGenericAllocationHandle* output, void* os_handle, CUmemAllocationHandleType type)
{
  return fakeCuMemImportFromShareableHandle(output, os_handle, type);
}

CUresult CUDAAPI
fakeCuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp* properties, CUmemGenericAllocationHandle handle)
{
  record("cuMemGetAllocationPropertiesFromHandle");
  last.pointer = properties;
  last.handle = handle;
  return (CUresult)FAKE_RESULT_PROPERTIES;
}
CUresult CUDAAPI
cuMemGetAllocationPropertiesFromHandle(CUmemAllocationProp* properties, CUmemGenericAllocationHandle handle)
{
  return fakeCuMemGetAllocationPropertiesFromHandle(properties, handle);
}

CUresult CUDAAPI
fakeCuMulticastCreate(CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  record("cuMulticastCreate");
  last.pointer = (void*)properties;
  last.size = properties != NULL ? properties->size : 0;
  if (output != NULL)
    *output = 0x456;
  return (CUresult)FAKE_RESULT_MULTICAST_CREATE;
}
CUresult CUDAAPI
cuMulticastCreate(CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  return fakeCuMulticastCreate(output, properties);
}

CUresult CUDAAPI
fakeCuMulticastAddDevice(CUmemGenericAllocationHandle multicast, CUdevice device)
{
  record("cuMulticastAddDevice");
  last.handle = multicast;
  last.device = device;
  last.has_device = 1;
  return (CUresult)FAKE_RESULT_MULTICAST_ADD_DEVICE;
}
CUresult CUDAAPI
cuMulticastAddDevice(CUmemGenericAllocationHandle multicast, CUdevice device)
{
  return fakeCuMulticastAddDevice(multicast, device);
}

CUresult CUDAAPI
fakeCuMulticastBindMem(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUmemGenericAllocationHandle memory,
    size_t memory_offset, size_t size, unsigned long long flags)
{
  record("cuMulticastBindMem");
  last.handle = multicast;
  last.offset = multicast_offset;
  last.memory = memory;
  last.memory_offset = memory_offset;
  last.size = size;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MULTICAST_BIND_MEM;
}
CUresult CUDAAPI
cuMulticastBindMem(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUmemGenericAllocationHandle memory,
    size_t memory_offset, size_t size, unsigned long long flags)
{
  return fakeCuMulticastBindMem(multicast, multicast_offset, memory, memory_offset, size, flags);
}

CUresult CUDAAPI
fakeCuMulticastBindAddr(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  record("cuMulticastBindAddr");
  last.handle = multicast;
  last.offset = multicast_offset;
  last.address = memory;
  last.size = size;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MULTICAST_BIND_ADDRESS;
}
CUresult CUDAAPI
cuMulticastBindAddr(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  return fakeCuMulticastBindAddr(multicast, multicast_offset, memory, size, flags);
}

#if CUDA_VERSION >= 13010
CUresult CUDAAPI
fakeCuMulticastBindMem_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size, unsigned long long flags)
{
  record("cuMulticastBindMem_v2");
  last.handle = multicast;
  last.device = device;
  last.has_device = 1;
  last.offset = multicast_offset;
  last.memory = memory;
  last.memory_offset = memory_offset;
  last.size = size;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MULTICAST_BIND_MEM_V2;
}
CUresult CUDAAPI
cuMulticastBindMem_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size, unsigned long long flags)
{
  return fakeCuMulticastBindMem_v2(multicast, device, multicast_offset, memory, memory_offset, size, flags);
}

CUresult CUDAAPI
fakeCuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  record("cuMulticastBindAddr_v2");
  last.handle = multicast;
  last.device = device;
  last.has_device = 1;
  last.offset = multicast_offset;
  last.address = memory;
  last.size = size;
  last.flags = flags;
  return (CUresult)FAKE_RESULT_MULTICAST_BIND_ADDRESS_V2;
}
CUresult CUDAAPI
cuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  return fakeCuMulticastBindAddr_v2(multicast, device, multicast_offset, memory, size, flags);
}
#endif

CUresult CUDAAPI
fakeCuMulticastGetGranularity(
    size_t* granularity, const CUmulticastObjectProp* properties, CUmulticastGranularity_flags option)
{
  record("cuMulticastGetGranularity");
  last.pointer = (void*)properties;
  last.flags = (unsigned long long)option;
  if (granularity != NULL)
    *granularity = 4096;
  return (CUresult)FAKE_RESULT_MULTICAST_GRANULARITY;
}
CUresult CUDAAPI
cuMulticastGetGranularity(
    size_t* granularity, const CUmulticastObjectProp* properties, CUmulticastGranularity_flags option)
{
  return fakeCuMulticastGetGranularity(granularity, properties, option);
}

CUresult CUDAAPI
fakeCuMulticastUnbind(CUmemGenericAllocationHandle multicast, CUdevice device, size_t offset, size_t size)
{
  record("cuMulticastUnbind");
  last.handle = multicast;
  last.device = device;
  last.has_device = 1;
  last.offset = offset;
  last.size = size;
  return (CUresult)FAKE_RESULT_MULTICAST_UNBIND;
}
CUresult CUDAAPI
cuMulticastUnbind(CUmemGenericAllocationHandle multicast, CUdevice device, size_t offset, size_t size)
{
  return fakeCuMulticastUnbind(multicast, device, offset, size);
}

CUresult CUDAAPI
cuCtxGetCurrent(CUcontext* context)
{
  *context = (CUcontext)(uintptr_t)1;
  return CUDA_SUCCESS;
}

/*
 * cuGetProcAddress in the real driver returns the entry point for the ABI the
 * caller asked for. This fake does the same for the two functions that have a
 * second ABI, and refuses versions newer than itself, like the real driver.
 */
void*
fakeOriginalForVersion(const char* symbol, int version)
{
  static const struct {
    const char* name;
    void* function;
  } table[] = {
      {"cuMemCreate", (void*)&fakeCuMemCreate},
      {"cuMemRelease", (void*)&fakeCuMemRelease},
      {"cuMemRetainAllocationHandle", (void*)&fakeCuMemRetainAllocationHandle},
      {"cuMemMap", (void*)&fakeCuMemMap},
      {"cuMemUnmap", (void*)&fakeCuMemUnmap},
      {"cuMemSetAccess", (void*)&fakeCuMemSetAccess},
      {"cuMemExportToShareableHandle", (void*)&fakeCuMemExportToShareableHandle},
      {"cuMemImportFromShareableHandle", (void*)&fakeCuMemImportFromShareableHandle},
      {"cuMemGetAllocationPropertiesFromHandle", (void*)&fakeCuMemGetAllocationPropertiesFromHandle},
      {"cuMulticastCreate", (void*)&fakeCuMulticastCreate},
      {"cuMulticastAddDevice", (void*)&fakeCuMulticastAddDevice},
      {"cuMulticastBindMem", (void*)&fakeCuMulticastBindMem},
      {"cuMulticastBindAddr", (void*)&fakeCuMulticastBindAddr},
#if CUDA_VERSION >= 13010
      {"cuMulticastBindMem_v2", (void*)&fakeCuMulticastBindMem_v2},
      {"cuMulticastBindAddr_v2", (void*)&fakeCuMulticastBindAddr_v2},
#endif
      {"cuMulticastGetGranularity", (void*)&fakeCuMulticastGetGranularity},
      {"cuMulticastUnbind", (void*)&fakeCuMulticastUnbind},
      {"cuCtxGetCurrent", (void*)&cuCtxGetCurrent},
      /* A local alias: a reference to the global name would bind to the shim. */
      {"cuGetProcAddress", (void*)&fakeCuGetProcAddress},
  };
  size_t index;

#if CUDA_VERSION >= 13010
  if (version >= 13010 && strcmp(symbol, "cuMulticastBindMem") == 0)
    return (void*)&fakeCuMulticastBindMem_v2;
  if (version >= 13010 && strcmp(symbol, "cuMulticastBindAddr") == 0)
    return (void*)&fakeCuMulticastBindAddr_v2;
#else
  (void)version;
#endif
  for (index = 0; index < sizeof(table) / sizeof(table[0]); index++) {
    if (strcmp(symbol, table[index].name) == 0)
      return table[index].function;
  }
  return NULL;
}

void*
fakeOriginal(const char* symbol)
{
  return fakeOriginalForVersion(symbol, CUDA_VERSION);
}

CUresult CUDAAPI
fakeCuGetProcAddress(const char* symbol, void** output, int version, cuuint64_t flags)
{
  (void)flags;
  if (version > CUDA_VERSION) {
    *output = NULL;
    return CUDA_ERROR_NOT_FOUND;
  }
  *output = fakeOriginalForVersion(symbol, version);
  return *output == NULL ? CUDA_ERROR_NOT_FOUND : CUDA_SUCCESS;
}
CUresult CUDAAPI
cuGetProcAddress(const char* symbol, void** output, int version, cuuint64_t flags)
{
  return fakeCuGetProcAddress(symbol, output, version, flags);
}

CUresult CUDAAPI
cuGetProcAddress_v2(
    const char* symbol, void** output, int version, cuuint64_t flags, CUdriverProcAddressQueryResult* status)
{
  CUresult result = fakeCuGetProcAddress(symbol, output, version, flags);
  if (status != NULL) {
    if (result == CUDA_SUCCESS)
      *status = CU_GET_PROC_ADDRESS_SUCCESS;
    else
      *status = version > CUDA_VERSION ? CU_GET_PROC_ADDRESS_VERSION_NOT_SUFFICIENT
                                       : CU_GET_PROC_ADDRESS_SYMBOL_NOT_FOUND;
  }
  return result;
}

CUresult CUDAAPI
cuGetProcAddress_v2_ptsz(
    const char* symbol, void** output, int version, cuuint64_t flags, CUdriverProcAddressQueryResult* status)
{
  return cuGetProcAddress_v2(symbol, output, version, flags, status);
}
