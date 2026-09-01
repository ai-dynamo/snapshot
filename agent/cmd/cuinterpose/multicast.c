/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "symbols.h"

#undef cuMulticastBindAddr
#undef cuMulticastBindMem

#define LOOKUP(name, type) ((type)cuinterposer_lookup_real_symbol(#name))

CUresult CUDAAPI
cuMulticastCreate(CUmemGenericAllocationHandle* output, const CUmulticastObjectProp* properties)
{
  typedef CUresult(CUDAAPI * function_type)(
      CUmemGenericAllocationHandle*, const CUmulticastObjectProp*);
  function_type function = LOOKUP(cuMulticastCreate, function_type);
  return function != NULL ? function(output, properties) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMulticastAddDevice(CUmemGenericAllocationHandle multicast, CUdevice device)
{
  typedef CUresult(CUDAAPI * function_type)(CUmemGenericAllocationHandle, CUdevice);
  function_type function = LOOKUP(cuMulticastAddDevice, function_type);
  return function != NULL ? function(multicast, device) : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMulticastBindMem(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size,
    unsigned long long flags)
{
  typedef CUresult(CUDAAPI * function_type)(
      CUmemGenericAllocationHandle, size_t, CUmemGenericAllocationHandle, size_t, size_t,
      unsigned long long);
  function_type function = LOOKUP(cuMulticastBindMem, function_type);
  return function != NULL
             ? function(multicast, multicast_offset, memory, memory_offset, size, flags)
             : cuinterposer_unavailable();
}

#if CUDA_VERSION >= 13010
CUresult CUDAAPI
cuMulticastBindMem_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset,
    CUmemGenericAllocationHandle memory, size_t memory_offset, size_t size,
    unsigned long long flags)
{
  typedef CUresult(CUDAAPI * function_type)(
      CUmemGenericAllocationHandle, CUdevice, size_t, CUmemGenericAllocationHandle,
      size_t, size_t, unsigned long long);
  function_type function = LOOKUP(cuMulticastBindMem_v2, function_type);
  return function != NULL
             ? function(
                   multicast, device, multicast_offset, memory, memory_offset, size, flags)
             : cuinterposer_unavailable();
}
#endif

CUresult CUDAAPI
cuMulticastBindAddr(
    CUmemGenericAllocationHandle multicast, size_t multicast_offset, CUdeviceptr memory,
    size_t size, unsigned long long flags)
{
  typedef CUresult(CUDAAPI * function_type)(
      CUmemGenericAllocationHandle, size_t, CUdeviceptr, size_t, unsigned long long);
  function_type function = LOOKUP(cuMulticastBindAddr, function_type);
  return function != NULL
             ? function(multicast, multicast_offset, memory, size, flags)
             : cuinterposer_unavailable();
}

#if CUDA_VERSION >= 13010
CUresult CUDAAPI
cuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t multicast_offset,
    CUdeviceptr memory, size_t size, unsigned long long flags)
{
  typedef CUresult(CUDAAPI * function_type)(
      CUmemGenericAllocationHandle, CUdevice, size_t, CUdeviceptr, size_t,
      unsigned long long);
  function_type function = LOOKUP(cuMulticastBindAddr_v2, function_type);
  return function != NULL
             ? function(multicast, device, multicast_offset, memory, size, flags)
             : cuinterposer_unavailable();
}
#endif

CUresult CUDAAPI
cuMulticastGetGranularity(
    size_t* granularity, const CUmulticastObjectProp* properties,
    CUmulticastGranularity_flags option)
{
  typedef CUresult(CUDAAPI * function_type)(
      size_t*, const CUmulticastObjectProp*, CUmulticastGranularity_flags);
  function_type function = LOOKUP(cuMulticastGetGranularity, function_type);
  return function != NULL ? function(granularity, properties, option)
                          : cuinterposer_unavailable();
}

CUresult CUDAAPI
cuMulticastUnbind(
    CUmemGenericAllocationHandle multicast, CUdevice device, size_t offset, size_t size)
{
  typedef CUresult(CUDAAPI * function_type)(
      CUmemGenericAllocationHandle, CUdevice, size_t, size_t);
  function_type function = LOOKUP(cuMulticastUnbind, function_type);
  return function != NULL ? function(multicast, device, offset, size)
                          : cuinterposer_unavailable();
}

#undef LOOKUP
