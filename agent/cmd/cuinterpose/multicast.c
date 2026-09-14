/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#define _GNU_SOURCE

#include "multicast.h"

#include <cuda.h>

#include "export.h"
#include "symbols.h"

/* cuda.h maps these names to versioned entry points; the shim wraps the names. */
#undef cuMulticastBindAddr
#undef cuMulticastBindMem

typedef CUresult(CUDAAPI* create_fn)(CUmemGenericAllocationHandle*, const CUmulticastObjectProp*);
typedef CUresult(CUDAAPI* add_device_fn)(CUmemGenericAllocationHandle, CUdevice);
typedef CUresult(CUDAAPI* bind_mem_fn)(
    CUmemGenericAllocationHandle, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
typedef CUresult(CUDAAPI* bind_address_fn)(CUmemGenericAllocationHandle, size_t, CUdeviceptr, size_t, unsigned long long);
#if CUDA_VERSION >= 13010
typedef CUresult(CUDAAPI* bind_mem_v2_fn)(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
typedef CUresult(CUDAAPI* bind_address_v2_fn)(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUdeviceptr, size_t, unsigned long long);
#endif
typedef CUresult(CUDAAPI* granularity_fn)(size_t*, const CUmulticastObjectProp*, CUmulticastGranularity_flags);
typedef CUresult(CUDAAPI* unbind_fn)(CUmemGenericAllocationHandle, CUdevice, size_t, size_t);

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastCreate(CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  create_fn function = (create_fn)cuinterpose_lookup_real_symbol("cuMulticastCreate");

  return function != NULL ? function(output, properties) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastAddDevice(CUmemGenericAllocationHandle multicast, CUdevice device)
{
  add_device_fn function = (add_device_fn)cuinterpose_lookup_real_symbol("cuMulticastAddDevice");

  return function != NULL ? function(multicast, device) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastBindMem(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUmemGenericAllocationHandle memory,
    size_t memory_offset, size_t size, unsigned long long flags)
{
  bind_mem_fn function = (bind_mem_fn)cuinterpose_lookup_real_symbol("cuMulticastBindMem");

  return function != NULL ? function(multicast, multicast_offset, memory, memory_offset, size, flags)
                          : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastBindAddr(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  bind_address_fn function = (bind_address_fn)cuinterpose_lookup_real_symbol("cuMulticastBindAddr");

  return function != NULL ? function(multicast, multicast_offset, memory, size, flags) : cuinterpose_unavailable();
}

#if CUDA_VERSION >= 13010
CUINTERPOSE_API CUresult CUDAAPI
cuMulticastBindMem_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size, unsigned long long flags)
{
  bind_mem_v2_fn function = (bind_mem_v2_fn)cuinterpose_lookup_real_symbol("cuMulticastBindMem_v2");

  return function != NULL ? function(multicast, device, multicast_offset, memory, memory_offset, size, flags)
                          : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset, CUdeviceptr memory, size_t size,
    unsigned long long flags)
{
  bind_address_v2_fn function = (bind_address_v2_fn)cuinterpose_lookup_real_symbol("cuMulticastBindAddr_v2");

  return function != NULL ? function(multicast, device, multicast_offset, memory, size, flags)
                          : cuinterpose_unavailable();
}
#endif

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastGetGranularity(
    size_t* granularity, const CUmulticastObjectProp* properties, CUmulticastGranularity_flags option)
{
  granularity_fn function = (granularity_fn)cuinterpose_lookup_real_symbol("cuMulticastGetGranularity");

  return function != NULL ? function(granularity, properties, option) : cuinterpose_unavailable();
}

CUINTERPOSE_API CUresult CUDAAPI
cuMulticastUnbind(CUmemGenericAllocationHandle multicast, CUdevice device, size_t offset, size_t size)
{
  unbind_fn function = (unbind_fn)cuinterpose_lookup_real_symbol("cuMulticastUnbind");

  return function != NULL ? function(multicast, device, offset, size) : cuinterpose_unavailable();
}
