/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

/* A stand-in libcudart.so.13: only the driver entry point resolvers. */

#include <cuda.h>
#include <cuda_runtime_api.h>

#include "fake_cuda.h"

#undef cudaGetDriverEntryPoint
#undef cudaGetDriverEntryPointByVersion

static cudaError_t
resolve(const char* symbol, void** output, unsigned int version, enum cudaDriverEntryPointQueryResult* status)
{
  *output = fakeOriginalForVersion(symbol, version == 0 ? CUDA_VERSION : (int)version);
  if (status != NULL)
    *status = *output != NULL ? cudaDriverEntryPointSuccess : cudaDriverEntryPointSymbolNotFound;
  return *output != NULL ? cudaSuccess : cudaErrorSymbolNotFound;
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPoint(
    const char* symbol, void** output, unsigned long long flags, enum cudaDriverEntryPointQueryResult* status)
{
  (void)flags;
  return resolve(symbol, output, 0, status);
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPoint_ptsz(
    const char* symbol, void** output, unsigned long long flags, enum cudaDriverEntryPointQueryResult* status)
{
  return cudaGetDriverEntryPoint(symbol, output, flags, status);
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPointByVersion(
    const char* symbol, void** output, unsigned int version, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  (void)flags;
  return resolve(symbol, output, version, status);
}

cudaError_t CUDARTAPI
cudaGetDriverEntryPointByVersion_ptsz(
    const char* symbol, void** output, unsigned int version, unsigned long long flags,
    enum cudaDriverEntryPointQueryResult* status)
{
  return cudaGetDriverEntryPointByVersion(symbol, output, version, flags, status);
}
