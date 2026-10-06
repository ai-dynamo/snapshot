// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// CPU-only hosts may not have libcuda. Load the driver when first needed.
#include <cuda.h>
#include <dlfcn.h>
#include <cstdio>
#include <string>

namespace {
void* Driver() {
  static void* library = [] {
    void* result = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
    if (!result) {
      const char* error = dlerror();
      // CUDA is optional on CPU nodes. Keep the loader diagnostic for operators.
      std::fprintf(stderr, "CUDA unavailable: %s\n", error ? error : "driver load failed");
    }
    return result;
  }();
  return library;
}
template <typename Function> Function Symbol(const char* name) {
  void* library = Driver();
  return library ? reinterpret_cast<Function>(dlsym(library, name)) : nullptr;
}
}  // namespace

extern "C" CUresult CUDAAPI cuInit(unsigned int flags) {
  if (!Driver()) return CUDA_ERROR_NO_DEVICE;
  static auto call = Symbol<decltype(&cuInit)>("cuInit");
  return call ? call(flags) : CUDA_ERROR_NOT_INITIALIZED;
}

#define STRINGIFY_SYMBOL_INNER(name) #name
#define STRINGIFY_SYMBOL(name) STRINGIFY_SYMBOL_INNER(name)
#define DRIVER_API(name, parameters, arguments) \
  extern "C" CUresult CUDAAPI name parameters { \
    static auto call = Symbol<decltype(&name)>(STRINGIFY_SYMBOL(name)); \
    return call ? call arguments : CUDA_ERROR_NOT_SUPPORTED; \
  }

DRIVER_API(cuGetErrorName, (CUresult status, const char** name), (status, name))
DRIVER_API(cuGetProcAddress, (const char* name, void** function, int version, cuuint64_t flags,
                            CUdriverProcAddressQueryResult* query), (name, function, version, flags, query))
DRIVER_API(cuDriverGetVersion, (int* version), (version))
DRIVER_API(cuDeviceGetCount, (int* count), (count))
DRIVER_API(cuDeviceGet, (CUdevice* device, int ordinal), (device, ordinal))
DRIVER_API(cuDeviceGetUuid, (CUuuid* uuid, CUdevice device), (uuid, device))
DRIVER_API(cuDevicePrimaryCtxRetain, (CUcontext* context, CUdevice device), (context, device))
DRIVER_API(cuCtxSetCurrent, (CUcontext context), (context))
DRIVER_API(cuCtxGetDevice, (CUdevice* device), (device))
DRIVER_API(cuDeviceGetAttribute, (int* value, CUdevice_attribute attribute, CUdevice device), (value, attribute, device))
DRIVER_API(cuStreamGetCtx, (CUstream stream, CUcontext* context), (stream, context))
DRIVER_API(cuCheckpointProcessGetState, (int pid, CUprocessState* state), (pid, state))
DRIVER_API(cuCheckpointProcessLock, (int pid, CUcheckpointLockArgs* args), (pid, args))
DRIVER_API(cuCheckpointProcessCheckpoint, (int pid, CUcheckpointCheckpointArgs* args), (pid, args))
DRIVER_API(cuCheckpointProcessRestore, (int pid, CUcheckpointRestoreArgs* args), (pid, args))
DRIVER_API(cuCheckpointProcessUnlock, (int pid, CUcheckpointUnlockArgs* args), (pid, args))
DRIVER_API(cuMemGetAllocationGranularity, (size_t* size, const CUmemAllocationProp* properties,
                                         CUmemAllocationGranularity_flags flags), (size, properties, flags))
DRIVER_API(cuMemCreate, (CUmemGenericAllocationHandle* handle, size_t size, const CUmemAllocationProp* properties,
                        unsigned long long flags), (handle, size, properties, flags))
DRIVER_API(cuMemRelease, (CUmemGenericAllocationHandle handle), (handle))
DRIVER_API(cuMemAddressReserve, (CUdeviceptr* address, size_t size, size_t alignment, CUdeviceptr requested,
                                unsigned long long flags), (address, size, alignment, requested, flags))
DRIVER_API(cuMemAddressFree, (CUdeviceptr address, size_t size), (address, size))
DRIVER_API(cuMemMap, (CUdeviceptr address, size_t size, size_t offset, CUmemGenericAllocationHandle handle,
                     unsigned long long flags), (address, size, offset, handle, flags))
DRIVER_API(cuMemUnmap, (CUdeviceptr address, size_t size), (address, size))
DRIVER_API(cuMemSetAccess, (CUdeviceptr address, size_t size, const CUmemAccessDesc* access, size_t count),
                         (address, size, access, count))
DRIVER_API(cuEventCreate, (CUevent* event, unsigned int flags), (event, flags))
DRIVER_API(cuEventDestroy, (CUevent event), (event))
DRIVER_API(cuEventRecord, (CUevent event, CUstream stream), (event, stream))
DRIVER_API(cuEventSynchronize, (CUevent event), (event))
DRIVER_API(cuStreamSynchronize, (CUstream stream), (stream))
DRIVER_API(cuMemcpyDtoHAsync, (void* host, CUdeviceptr device, size_t size, CUstream stream), (host, device, size, stream))
DRIVER_API(cuMemcpyHtoDAsync, (CUdeviceptr device, const void* host, size_t size, CUstream stream), (device, host, size, stream))

#undef DRIVER_API
#undef STRINGIFY_SYMBOL
#undef STRINGIFY_SYMBOL_INNER
