/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include <cuda.h>
#include <cuda_runtime_api.h>
#include <string.h>

extern CUresult CUDAAPI fakeCuMemCreateOriginal(
    CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);

static cudaError_t
resolve(const char* symbol, void** output, enum cudaDriverEntryPointQueryResult* status)
{
  *output = strcmp(symbol, "cuMemCreate") == 0 ? (void*)&fakeCuMemCreateOriginal : NULL;
  if (status != NULL)
    *status = *output != NULL ? cudaDriverEntryPointSuccess : cudaDriverEntryPointSymbolNotFound;
  return *output != NULL ? cudaSuccess : cudaErrorSymbolNotFound;
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPoint(
    const char* symbol, void** output, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  (void)flags;
  return resolve(symbol, output, status);
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPoint_ptsz(
    const char* symbol, void** output, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  return cudaGetDriverEntryPoint(symbol, output, flags, status);
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPointByVersion(
    const char* symbol, void** output, unsigned int version, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  (void)version;
  return cudaGetDriverEntryPoint(symbol, output, flags, status);
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPointByVersion_ptsz(
    const char* symbol, void** output, unsigned int version, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  return cudaGetDriverEntryPointByVersion(symbol, output, version, flags, status);
}
