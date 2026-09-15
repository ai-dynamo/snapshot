// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once
#include "../core_abi.h"
#include "protocol.hpp"
#include <stdexcept>

namespace cuinterpose {
constexpr int success = 0, invalid_value = 1, out_of_memory = 2,
    not_initialized = 3, invalid_handle = 400, not_ready = 600,
    not_supported = 801, unknown = 999;
struct CudaError { int code; };
inline void check(int status) { if (status != success) throw CudaError{status}; }
extern Host host;

namespace driver {
// Resolve through the frontend's provider policy; never link against libcuda.
#define FUNCTION(name, parameters, arguments) \
 inline int name parameters { \
   using Function = int (*) parameters; \
   auto function = reinterpret_cast<Function>(host.resolve(#name)); \
   if (!function) return not_initialized; \
   return function arguments; \
 }
CUINTERPOSE_MEMORY_API(FUNCTION)
FUNCTION(cuCtxGetCurrent, (void** context), (context))
FUNCTION(cuCtxGetDevice, (int32_t* device), (device))
FUNCTION(cuCtxSetCurrent, (void* context), (context))
FUNCTION(cuDevicePrimaryCtxRetain, (void** context, int32_t device), (context, device))
FUNCTION(cuDevicePrimaryCtxRelease_v2, (int32_t device), (device))
FUNCTION(cuMemAddressReserve, (uint64_t* address, size_t size, size_t alignment, uint64_t requested, uint64_t flags), (address,size,alignment,requested,flags))
FUNCTION(cuMemAddressFree, (uint64_t address, size_t size), (address,size))
FUNCTION(cuMemHostRegister_v2, (void* address, size_t size, uint32_t flags), (address,size,flags))
FUNCTION(cuMemHostUnregister, (void* address), (address))
FUNCTION(cuMemHostGetFlags, (uint32_t* flags, void* address), (flags,address))
FUNCTION(cuMemcpyDtoHAsync_v2, (void* host_memory, uint64_t device, size_t size, void* stream), (host_memory,device,size,stream))
FUNCTION(cuMemcpyHtoDAsync_v2, (uint64_t device, const void* host_memory, size_t size, void* stream), (device,host_memory,size,stream))
FUNCTION(cuStreamCreate, (void** stream, uint32_t flags), (stream,flags))
FUNCTION(cuStreamSynchronize, (void* stream), (stream))
FUNCTION(cuStreamDestroy_v2, (void* stream), (stream))
#undef FUNCTION
Fd export_posix(uint64_t);
uint64_t import_posix(int);
}
}
