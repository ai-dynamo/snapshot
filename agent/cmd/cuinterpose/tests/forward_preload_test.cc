// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Runs with LD_PRELOAD=libcuinterpose.so against the fake libcuda.so.1 and
// libcudart.so.13 in the same directory. Proves that every CUDA entry point the
// shim replaces is reached through all four resolution paths and that the call
// arrives at the driver with its arguments unchanged.

#include <cuda.h>
#include <cuda_runtime_api.h>
#include <dlfcn.h>
#include <gtest/gtest.h>

#include <cstring>
#include <string>

#include "../export.h"
#include "fake_cuda.h"

#undef cuGetProcAddress
#undef cuMulticastBindAddr
#undef cuMulticastBindMem
#undef cudaGetDriverEntryPoint
#undef cudaGetDriverEntryPointByVersion

extern "C" {
CUresult CUDAAPI cuGetProcAddress(const char*, void**, int, cuuint64_t);
CUresult CUDAAPI cuGetProcAddress_v2(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
CUresult CUDAAPI cuGetProcAddress_v2_ptsz(const char*, void**, int, cuuint64_t, CUdriverProcAddressQueryResult*);
cudaError_t CUDARTAPI cudaGetDriverEntryPoint(const char*, void**, unsigned long long, cudaDriverEntryPointQueryResult*);
cudaError_t CUDARTAPI cudaGetDriverEntryPointByVersion(
    const char*, void**, unsigned int, unsigned long long, cudaDriverEntryPointQueryResult*);
cudaError_t CUDARTAPI cudaGetDriverEntryPoint_ptsz(
    const char*, void**, unsigned long long, cudaDriverEntryPointQueryResult*);
cudaError_t CUDARTAPI cudaGetDriverEntryPointByVersion_ptsz(
    const char*, void**, unsigned int, unsigned long long, cudaDriverEntryPointQueryResult*);
CUresult CUDAAPI cuMulticastBindMem(
    CUmemGenericAllocationHandle, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
CUresult CUDAAPI cuMulticastBindAddr(CUmemGenericAllocationHandle, size_t, CUdeviceptr, size_t, unsigned long long);
#if CUDA_VERSION >= 13010
CUresult CUDAAPI cuMulticastBindMem_v2(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUmemGenericAllocationHandle, size_t, size_t, unsigned long long);
CUresult CUDAAPI cuMulticastBindAddr_v2(
    CUmemGenericAllocationHandle, CUdevice, size_t, CUdeviceptr, size_t, unsigned long long);
#endif
}

namespace {

// The shim's own copy of a wrapper, as seen from this executable. Direct calls
// in this file bind to the same address because LD_PRELOAD comes first.
void* shim(const char* name) { return dlsym(RTLD_DEFAULT, name); }

class Forwarding : public ::testing::Test {
 protected:
  void SetUp() override { fakeReset(); }
  const fake_last_call& last() const { return *fakeLastCall(); }
};

TEST_F(Forwarding, ShimIsLoadedAheadOfTheDriver) {
  ASSERT_NE(shim("cuMemCreate"), nullptr);
  EXPECT_NE(shim("cuMemCreate"), fakeOriginal("cuMemCreate")) << "LD_PRELOAD did not put the shim first";
  auto* info = static_cast<const struct cuinterpose_build_info*>(dlsym(RTLD_DEFAULT, "cuinterpose_build_info"));
  ASSERT_NE(info, nullptr);
  EXPECT_EQ(info->cuda_version, static_cast<uint32_t>(CUDA_VERSION));
}

TEST_F(Forwarding, UnicastEntryPointsForwardArguments) {
  CUmemAllocationProp prop{};
  prop.type = CU_MEM_ALLOCATION_TYPE_PINNED;
  prop.requestedHandleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  CUmemGenericAllocationHandle handle = 0;
  EXPECT_EQ(cuMemCreate(&handle, 4096, &prop, 7), static_cast<CUresult>(FAKE_RESULT_CREATE));
  EXPECT_STREQ(last().function, "cuMemCreate");
  EXPECT_EQ(last().size, 4096u);
  EXPECT_EQ(last().flags, 7u);
  EXPECT_EQ(last().handle_type, static_cast<int>(CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR));
  EXPECT_EQ(handle, 0u) << "on a driver error the output is left untouched";

  EXPECT_EQ(cuMemRelease(0x77), static_cast<CUresult>(FAKE_RESULT_RELEASE));
  EXPECT_EQ(last().handle, 0x77u);

  CUmemGenericAllocationHandle retained = 0;
  EXPECT_EQ(cuMemRetainAllocationHandle(&retained, reinterpret_cast<void*>(0x1000)),
            static_cast<CUresult>(FAKE_RESULT_RETAIN));
  EXPECT_EQ(last().pointer, reinterpret_cast<void*>(0x1000));
  EXPECT_EQ(retained, 0u) << "on a driver error the output is left untouched";

  EXPECT_EQ(cuMemMap(0x2000, 8192, 16, 0xabc, 3), static_cast<CUresult>(FAKE_RESULT_MAP));
  EXPECT_EQ(last().address, 0x2000u);
  EXPECT_EQ(last().size, 8192u);
  EXPECT_EQ(last().offset, 16u);
  EXPECT_EQ(last().handle, 0xabcu);
  EXPECT_EQ(last().flags, 3u);

  EXPECT_EQ(cuMemUnmap(0x2000, 8192), static_cast<CUresult>(FAKE_RESULT_UNMAP));
  EXPECT_EQ(last().address, 0x2000u);

  CUmemAccessDesc access[2]{};
  EXPECT_EQ(cuMemSetAccess(0x2000, 8192, access, 2), static_cast<CUresult>(FAKE_RESULT_ACCESS));
  EXPECT_EQ(last().count, 2u);
  EXPECT_EQ(last().pointer, static_cast<void*>(access));

  int fd = -1;
  EXPECT_EQ(cuMemExportToShareableHandle(&fd, 0xabc, CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR, 0),
            static_cast<CUresult>(FAKE_RESULT_EXPORT));
  EXPECT_EQ(last().pointer, static_cast<void*>(&fd));
  EXPECT_EQ(last().handle, 0xabcu);

  CUmemGenericAllocationHandle imported = 0;
  EXPECT_EQ(cuMemImportFromShareableHandle(&imported, reinterpret_cast<void*>(42), CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR),
            static_cast<CUresult>(FAKE_RESULT_IMPORT));
  EXPECT_EQ(last().pointer, reinterpret_cast<void*>(42));
  EXPECT_EQ(imported, 0u) << "on a driver error the output is left untouched";

  EXPECT_EQ(cuMemGetAllocationPropertiesFromHandle(&prop, 0x55), static_cast<CUresult>(FAKE_RESULT_PROPERTIES));
  EXPECT_EQ(last().handle, 0x55u);
}

TEST_F(Forwarding, MulticastEntryPointsForwardArguments) {
  CUmulticastObjectProp prop{};
  prop.numDevices = 2;
  prop.size = 1 << 20;
  prop.handleTypes = CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR;
  CUmemGenericAllocationHandle mc = 0;
  EXPECT_EQ(cuMulticastCreate(&mc, &prop), static_cast<CUresult>(FAKE_RESULT_MULTICAST_CREATE));
  EXPECT_EQ(last().size, prop.size);
  EXPECT_EQ(mc, 0x456u);

  EXPECT_EQ(cuMulticastAddDevice(mc, 1), static_cast<CUresult>(FAKE_RESULT_MULTICAST_ADD_DEVICE));
  EXPECT_EQ(last().device, 1);

  EXPECT_EQ(cuMulticastBindMem(mc, 64, 0xabc, 128, 4096, 5), static_cast<CUresult>(FAKE_RESULT_MULTICAST_BIND_MEM));
  EXPECT_STREQ(last().function, "cuMulticastBindMem");
  EXPECT_EQ(last().offset, 64u);
  EXPECT_EQ(last().memory, 0xabcu);
  EXPECT_EQ(last().memory_offset, 128u);
  EXPECT_EQ(last().size, 4096u);
  EXPECT_EQ(last().flags, 5u);
  EXPECT_EQ(last().has_device, 0);

  EXPECT_EQ(cuMulticastBindAddr(mc, 64, 0x3000, 4096, 6), static_cast<CUresult>(FAKE_RESULT_MULTICAST_BIND_ADDRESS));
  EXPECT_EQ(last().address, 0x3000u);

#if CUDA_VERSION >= 13010
  EXPECT_EQ(cuMulticastBindMem_v2(mc, 3, 64, 0xabc, 128, 4096, 5),
            static_cast<CUresult>(FAKE_RESULT_MULTICAST_BIND_MEM_V2));
  EXPECT_STREQ(last().function, "cuMulticastBindMem_v2");
  EXPECT_EQ(last().device, 3);
  EXPECT_EQ(last().has_device, 1);
  EXPECT_EQ(cuMulticastBindAddr_v2(mc, 4, 64, 0x3000, 4096, 6),
            static_cast<CUresult>(FAKE_RESULT_MULTICAST_BIND_ADDRESS_V2));
  EXPECT_EQ(last().device, 4);
#endif

  size_t granularity = 0;
  EXPECT_EQ(cuMulticastGetGranularity(&granularity, &prop, CU_MULTICAST_GRANULARITY_RECOMMENDED),
            static_cast<CUresult>(FAKE_RESULT_MULTICAST_GRANULARITY));
  EXPECT_EQ(granularity, 4096u);

  EXPECT_EQ(cuMulticastUnbind(mc, 1, 64, 4096), static_cast<CUresult>(FAKE_RESULT_MULTICAST_UNBIND));
  EXPECT_EQ(last().device, 1);
  EXPECT_EQ(last().offset, 64u);
}

// A driver symbol the shim does not wrap goes straight through.
TEST_F(Forwarding, UnwrappedDriverSymbolsAreUntouched) {
  void* cuda = dlopen("libcuda.so.1", RTLD_NOW);
  ASSERT_NE(cuda, nullptr);
  EXPECT_EQ(dlsym(cuda, "cuCtxGetCurrent"), fakeOriginal("cuCtxGetCurrent"));
  void* entry = nullptr;
  EXPECT_EQ(cuGetProcAddress("cuCtxGetCurrent", &entry, CUDA_VERSION, 0), CUDA_SUCCESS);
  EXPECT_EQ(entry, fakeOriginal("cuCtxGetCurrent"));
  // Non-CUDA lookups through the replaced dlsym are untouched too.
  EXPECT_EQ(dlsym(RTLD_DEFAULT, "malloc"), reinterpret_cast<void*>(&malloc));
  dlclose(cuda);
}

TEST_F(Forwarding, DlsymOnTheDriverHandleReturnsTheShim) {
  void* cuda = dlopen("libcuda.so.1", RTLD_NOW);
  ASSERT_NE(cuda, nullptr);
  for (const char* name : {"cuMemCreate", "cuMemMap", "cuMemExportToShareableHandle", "cuMulticastCreate",
                           "cuMulticastBindMem", "cuGetProcAddress"}) {
    void* symbol = dlsym(cuda, name);
    ASSERT_NE(symbol, nullptr) << name;
    EXPECT_EQ(symbol, shim(name)) << name << " resolved to the driver, not the shim";
    EXPECT_NE(symbol, fakeOriginal(name)) << name;
  }
  dlclose(cuda);
}

TEST_F(Forwarding, DlsymOnTheRuntimeHandleReturnsTheShim) {
  void* cudart = dlopen("libcudart.so.13", RTLD_NOW);
  ASSERT_NE(cudart, nullptr);
  for (const char* name : {"cudaGetDriverEntryPoint", "cudaGetDriverEntryPointByVersion",
                           "cudaGetDriverEntryPoint_ptsz", "cudaGetDriverEntryPointByVersion_ptsz"}) {
    EXPECT_EQ(dlsym(cudart, name), shim(name)) << name;
  }
  dlclose(cudart);
}

TEST_F(Forwarding, CuGetProcAddressSubstitutesAllVersions) {
  for (const char* name : {"cuMemCreate", "cuMemRelease", "cuMemRetainAllocationHandle", "cuMemMap", "cuMemUnmap",
                           "cuMemSetAccess", "cuMemExportToShareableHandle", "cuMemImportFromShareableHandle",
                           "cuMemGetAllocationPropertiesFromHandle", "cuMulticastCreate", "cuMulticastAddDevice",
                           "cuMulticastGetGranularity", "cuMulticastUnbind"}) {
    void* v1 = nullptr;
    EXPECT_EQ(cuGetProcAddress(name, &v1, 12000, 0), CUDA_SUCCESS) << name;
    EXPECT_EQ(v1, shim(name)) << name << " via cuGetProcAddress";

    void* v2 = nullptr;
    CUdriverProcAddressQueryResult status{};
    EXPECT_EQ(cuGetProcAddress_v2(name, &v2, CUDA_VERSION, 0, &status), CUDA_SUCCESS) << name;
    EXPECT_EQ(status, CU_GET_PROC_ADDRESS_SUCCESS);
    EXPECT_EQ(v2, shim(name)) << name << " via cuGetProcAddress_v2";

    void* ptsz = nullptr;
    EXPECT_EQ(cuGetProcAddress_v2_ptsz(name, &ptsz, CUDA_VERSION, 0, &status), CUDA_SUCCESS) << name;
    EXPECT_EQ(ptsz, shim(name)) << name << " via cuGetProcAddress_v2_ptsz";
  }
}

TEST_F(Forwarding, CuGetProcAddressResolvesItself) {
  void* entry = nullptr;
  EXPECT_EQ(cuGetProcAddress("cuGetProcAddress", &entry, 11000, 0), CUDA_SUCCESS);
  EXPECT_EQ(entry, shim("cuGetProcAddress")) << "pre-12.0 callers get the four-argument ABI";
  EXPECT_EQ(cuGetProcAddress("cuGetProcAddress", &entry, 12000, 0), CUDA_SUCCESS);
  EXPECT_EQ(entry, shim("cuGetProcAddress_v2")) << "12.0+ callers get the five-argument ABI";
}

TEST_F(Forwarding, CuGetProcAddressSelectsTheBindAbiByVersion) {
  void* entry = nullptr;
  EXPECT_EQ(cuGetProcAddress("cuMulticastBindMem", &entry, 12000, 0), CUDA_SUCCESS);
  EXPECT_EQ(entry, shim("cuMulticastBindMem"));
  EXPECT_EQ(cuGetProcAddress("cuMulticastBindAddr", &entry, 12000, 0), CUDA_SUCCESS);
  EXPECT_EQ(entry, shim("cuMulticastBindAddr"));
#if CUDA_VERSION >= 13010
  EXPECT_EQ(cuGetProcAddress("cuMulticastBindMem", &entry, 13010, 0), CUDA_SUCCESS);
  EXPECT_EQ(entry, shim("cuMulticastBindMem_v2")) << "a 13.1 caller must get the device-explicit ABI";
  EXPECT_EQ(cuGetProcAddress("cuMulticastBindAddr", &entry, 13010, 0), CUDA_SUCCESS);
  EXPECT_EQ(entry, shim("cuMulticastBindAddr_v2"));
  // And the substituted wrapper really has the seven-argument signature.
  using bind_v2 = CUresult(CUDAAPI*)(CUmemGenericAllocationHandle, CUdevice, size_t, CUmemGenericAllocationHandle,
                                     size_t, size_t, unsigned long long);
  EXPECT_EQ(cuGetProcAddress("cuMulticastBindMem", &entry, 13010, 0), CUDA_SUCCESS);
  EXPECT_EQ(reinterpret_cast<bind_v2>(entry)(0x456, 2, 0, 0xabc, 0, 4096, 0),
            static_cast<CUresult>(FAKE_RESULT_MULTICAST_BIND_MEM_V2));
  EXPECT_EQ(last().device, 2);
#endif
}

TEST_F(Forwarding, CuGetProcAddressFailureIsNotSubstituted) {
  void* entry = reinterpret_cast<void*>(1);
  CUdriverProcAddressQueryResult status{};
  EXPECT_EQ(cuGetProcAddress_v2("cuMemCreate", &entry, fakeDriverVersion() + 1000, 0, &status), CUDA_ERROR_NOT_FOUND);
  EXPECT_EQ(status, CU_GET_PROC_ADDRESS_VERSION_NOT_SUFFICIENT);
  EXPECT_EQ(entry, nullptr) << "a driver error must not be turned into a shim wrapper";
  EXPECT_EQ(cuGetProcAddress("cuDoesNotExist", &entry, CUDA_VERSION, 0), CUDA_ERROR_NOT_FOUND);
  EXPECT_EQ(entry, nullptr);
}

TEST_F(Forwarding, RuntimeResolversSubstitute) {
  for (const char* name : {"cuMemCreate", "cuMemMap", "cuMulticastCreate", "cuMulticastAddDevice"}) {
    void* entry = nullptr;
    cudaDriverEntryPointQueryResult status{};
    EXPECT_EQ(cudaGetDriverEntryPoint(name, &entry, cudaEnableDefault, &status), cudaSuccess) << name;
    EXPECT_EQ(status, cudaDriverEntryPointSuccess);
    EXPECT_EQ(entry, shim(name)) << name << " via cudaGetDriverEntryPoint";
    entry = nullptr;
    EXPECT_EQ(cudaGetDriverEntryPoint_ptsz(name, &entry, cudaEnableDefault, &status), cudaSuccess) << name;
    EXPECT_EQ(entry, shim(name)) << name << " via cudaGetDriverEntryPoint_ptsz";
    entry = nullptr;
    EXPECT_EQ(cudaGetDriverEntryPointByVersion(name, &entry, 12000, cudaEnableDefault, &status), cudaSuccess) << name;
    EXPECT_EQ(entry, shim(name)) << name << " via cudaGetDriverEntryPointByVersion";
    entry = nullptr;
    EXPECT_EQ(cudaGetDriverEntryPointByVersion_ptsz(name, &entry, 12000, cudaEnableDefault, &status), cudaSuccess)
        << name;
    EXPECT_EQ(entry, shim(name)) << name << " via cudaGetDriverEntryPointByVersion_ptsz";
  }
  void* entry = reinterpret_cast<void*>(1);
  cudaDriverEntryPointQueryResult status{};
  EXPECT_EQ(cudaGetDriverEntryPoint("cuDoesNotExist", &entry, cudaEnableDefault, &status), cudaErrorSymbolNotFound);
  EXPECT_EQ(status, cudaDriverEntryPointSymbolNotFound);
  EXPECT_EQ(entry, nullptr);
#if CUDA_VERSION >= 13010
  EXPECT_EQ(cudaGetDriverEntryPointByVersion("cuMulticastBindMem", &entry, 13010, cudaEnableDefault, &status), cudaSuccess);
  EXPECT_EQ(entry, shim("cuMulticastBindMem_v2"));
#endif
}

// The substituted entry point behaves like a direct call.
TEST_F(Forwarding, SubstitutedEntryPointForwards) {
  using create_fn = CUresult(CUDAAPI*)(CUmemGenericAllocationHandle*, size_t, const CUmemAllocationProp*, unsigned long long);
  void* entry = nullptr;
  ASSERT_EQ(cuGetProcAddress("cuMemCreate", &entry, CUDA_VERSION, 0), CUDA_SUCCESS);
  CUmemGenericAllocationHandle handle = 0;
  EXPECT_EQ(reinterpret_cast<create_fn>(entry)(&handle, 123, nullptr, 0), static_cast<CUresult>(FAKE_RESULT_CREATE));
  EXPECT_EQ(last().size, 123u);
  EXPECT_EQ(last().handle_type, -1);
}

}  // namespace
