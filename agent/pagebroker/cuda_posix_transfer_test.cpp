// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Exercise the real chunked POSIX pipeline without a GPU. CUDA copies
// use host addresses; this does not qualify driver import/map behavior.
#include "file_descriptor.hpp"
#include "cuda_posix_transfer.hpp"

#include <gtest/gtest.h>
#include <sys/mman.h>
#include <unistd.h>
#include <cstdlib>
#include <cstring>
#include <map>
#include <set>
#include <vector>

namespace {
int allocation_calls = 0;
int host_numa = 7;
constexpr size_t allocation_granularity = 2 * 1024 * 1024;
std::set<CUmemGenericAllocationHandle> allocations;
std::map<CUdeviceptr, size_t> reservations;
std::set<CUdeviceptr> mappings;
int events = 0;
enum class AllocationFailure { None, Create, Reserve, Map, Access, Event };
AllocationFailure allocation_failure = AllocationFailure::None;
bool fail_copy = false;
int stream_drains = 0;
constexpr size_t chunk_bytes = 65536;
}

extern "C" {
CUresult CUDAAPI cuGetErrorName(CUresult, const char** name)
{
  *name = "fake CUDA error";
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuCtxSetCurrent(CUcontext) { return CUDA_SUCCESS; }
CUresult CUDAAPI cuCtxGetDevice(CUdevice* device)
{
  *device = 2;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuDeviceGetAttribute(int* value, CUdevice_attribute attribute, CUdevice device)
{
  EXPECT_EQ(device, 2);
  EXPECT_EQ(attribute, CU_DEVICE_ATTRIBUTE_HOST_NUMA_ID);
  *value = host_numa;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemGetAllocationGranularity(size_t* granularity, const CUmemAllocationProp* prop,
                                              CUmemAllocationGranularity_flags flags)
{
  EXPECT_EQ(prop->type, CU_MEM_ALLOCATION_TYPE_PINNED);
  EXPECT_EQ(prop->location.type, CU_MEM_LOCATION_TYPE_HOST_NUMA);
  EXPECT_EQ(prop->location.id, host_numa == -1 ? 0 : host_numa);
  EXPECT_EQ(flags, CU_MEM_ALLOC_GRANULARITY_MINIMUM);
  *granularity = allocation_granularity;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemCreate(CUmemGenericAllocationHandle* handle, size_t size,
                            const CUmemAllocationProp* prop, unsigned long long)
{
  if (allocation_failure == AllocationFailure::Create) return CUDA_ERROR_NOT_SUPPORTED;
  EXPECT_EQ(size % allocation_granularity, 0);
  EXPECT_EQ(prop->location.type, CU_MEM_LOCATION_TYPE_HOST_NUMA);
  EXPECT_EQ(prop->location.id, host_numa == -1 ? 0 : host_numa);
  *handle = ++allocation_calls;
  allocations.insert(*handle);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemRelease(CUmemGenericAllocationHandle handle)
{
  EXPECT_EQ(allocations.erase(handle), 1);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemAddressReserve(CUdeviceptr* address, size_t size, size_t alignment,
                                    CUdeviceptr, unsigned long long)
{
  if (allocation_failure == AllocationFailure::Reserve) return CUDA_ERROR_OUT_OF_MEMORY;
  void* data = nullptr;
  if (posix_memalign(&data, alignment, size)) return CUDA_ERROR_OUT_OF_MEMORY;
  *address = reinterpret_cast<CUdeviceptr>(data);
  reservations[*address] = size;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemAddressFree(CUdeviceptr address, size_t size)
{
  EXPECT_EQ(mappings.count(address), 0);
  EXPECT_EQ(reservations.at(address), size);
  reservations.erase(address);
  free(reinterpret_cast<void*>(address));
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemMap(CUdeviceptr address, size_t size, size_t offset,
                         CUmemGenericAllocationHandle handle, unsigned long long)
{
  if (allocation_failure == AllocationFailure::Map) return CUDA_ERROR_OUT_OF_MEMORY;
  EXPECT_EQ(reservations.at(address), size);
  EXPECT_EQ(allocations.count(handle), 1);
  EXPECT_EQ(offset, 0);
  mappings.insert(address);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemUnmap(CUdeviceptr address, size_t size)
{
  EXPECT_EQ(reservations.at(address), size);
  EXPECT_EQ(mappings.erase(address), 1);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemSetAccess(CUdeviceptr address, size_t size,
                               const CUmemAccessDesc* access, size_t count)
{
  if (allocation_failure == AllocationFailure::Access) return CUDA_ERROR_NOT_SUPPORTED;
  EXPECT_EQ(mappings.count(address), 1);
  EXPECT_EQ(reservations.at(address), size);
  EXPECT_EQ(count, 2);
  EXPECT_EQ(access[0].location.type, CU_MEM_LOCATION_TYPE_HOST_NUMA);
  EXPECT_EQ(access[0].flags, CU_MEM_ACCESS_FLAGS_PROT_READWRITE);
  EXPECT_EQ(access[1].location.type, CU_MEM_LOCATION_TYPE_DEVICE);
  EXPECT_EQ(access[1].location.id, 2);
  EXPECT_EQ(access[1].flags, CU_MEM_ACCESS_FLAGS_PROT_READWRITE);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuEventCreate(CUevent* event, unsigned int)
{
  if (allocation_failure == AllocationFailure::Event) return CUDA_ERROR_OUT_OF_MEMORY;
  ++events;
  *event = reinterpret_cast<CUevent>(1);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuEventDestroy(CUevent)
{
  --events;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuEventSynchronize(CUevent) { return CUDA_SUCCESS; }
CUresult CUDAAPI cuEventRecord(CUevent, CUstream) { return CUDA_SUCCESS; }
CUresult CUDAAPI cuStreamSynchronize(CUstream) { ++stream_drains; return CUDA_SUCCESS; }
CUresult CUDAAPI cuMemcpyDtoHAsync(void* to, CUdeviceptr from, size_t size, CUstream)
{
  if (fail_copy)
    return CUDA_ERROR_INVALID_VALUE;
  std::memcpy(to, reinterpret_cast<const void*>(from), size);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemcpyHtoDAsync(CUdeviceptr to, const void* from, size_t size, CUstream)
{
  if (fail_copy)
    return CUDA_ERROR_INVALID_VALUE;
  std::memcpy(reinterpret_cast<void*>(to), from, size);
  return CUDA_SUCCESS;
}
}

TEST(CudaTransfer, ReusesRingAcrossFilesAndPartialFinalChunks)
{
  namespace transfer = snapshot::pagebroker::cuda;
  const size_t size = 17 * chunk_bytes + 13;
  std::vector<unsigned char> source(size), restored(size);
  for (size_t i = 0; i < size; ++i) source[i] = (i * 7 + i / 4096) % 251;
  const int before = allocation_calls;
  {
    transfer::TransferBuffers buffers({4, chunk_bytes});
    std::string error;
    ASSERT_TRUE(buffers.Initialize(reinterpret_cast<CUcontext>(1), &error)) << error;
    for (int iteration = 0; iteration < 3; ++iteration) {
      FileDescriptor file(memfd_create("ring", MFD_CLOEXEC));
      ASSERT_EQ(ftruncate(file.get(), size), 0);
      transfer::TransferMetrics saved, loaded;
      auto stream = reinterpret_cast<CUstream>(1);
      ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(source.data()), size,
                                  stream, transfer::TransferOperation::kCheckpoint, &saved, &error)) << error;
      ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(restored.data()), size,
                                  stream, transfer::TransferOperation::kRestore, &loaded, &error)) << error;
      EXPECT_EQ(restored, source);
      EXPECT_EQ(saved.bytes, size);
      EXPECT_EQ(loaded.bytes, size);
      EXPECT_EQ(allocation_calls - before, 4);
    }
  }
  EXPECT_TRUE(allocations.empty());
  EXPECT_TRUE(reservations.empty());
  EXPECT_TRUE(mappings.empty());
  EXPECT_EQ(events, 0);
}

TEST(CudaTransfer, DrainsFailedCopyBeforeReusingRing)
{
  namespace transfer = snapshot::pagebroker::cuda;
  const size_t size = 5 * chunk_bytes;
  std::vector<unsigned char> source(size, 91), restored(size);
  FileDescriptor file(memfd_create("copy-failure", MFD_CLOEXEC));
  ASSERT_EQ(ftruncate(file.get(), size), 0);
  transfer::TransferBuffers buffers({2, chunk_bytes});
  transfer::TransferMetrics metrics;
  std::string error;
  ASSERT_TRUE(buffers.Initialize(reinterpret_cast<CUcontext>(1), &error)) << error;
  auto stream = reinterpret_cast<CUstream>(1);
  const int before = stream_drains;
  fail_copy = true;
  EXPECT_FALSE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(source.data()), size,
                               stream, transfer::TransferOperation::kCheckpoint, &metrics, &error));
  fail_copy = false;
  EXPECT_EQ(stream_drains, before + 1);
  ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(source.data()), size,
                              stream, transfer::TransferOperation::kCheckpoint, &metrics, &error)) << error;
  ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(restored.data()), size,
                              stream, transfer::TransferOperation::kRestore, &metrics, &error)) << error;
  EXPECT_EQ(restored, source);
}

TEST(CudaTransfer, FailedReadDoesNotPoisonNextFile)
{
  namespace transfer = snapshot::pagebroker::cuda;
  const size_t size = 3 * chunk_bytes;
  std::vector<unsigned char> source(size, 19), restored(size);
  transfer::TransferBuffers buffers({2, chunk_bytes});
  transfer::TransferMetrics metrics;
  std::string error;
  ASSERT_TRUE(buffers.Initialize(reinterpret_cast<CUcontext>(1), &error)) << error;
  auto stream = reinterpret_cast<CUstream>(1);
  FileDescriptor empty(memfd_create("empty", MFD_CLOEXEC));
  bool failed = false;
  try {
    failed = !buffers.Transfer(empty.get(), reinterpret_cast<CUdeviceptr>(restored.data()), size,
                               stream, transfer::TransferOperation::kRestore, &metrics, &error);
  } catch (const std::exception&) { failed = true; }
  EXPECT_TRUE(failed);
  FileDescriptor file(memfd_create("after-failure", MFD_CLOEXEC));
  ASSERT_EQ(ftruncate(file.get(), size), 0);
  ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(source.data()), size,
                              stream, transfer::TransferOperation::kCheckpoint, &metrics, &error)) << error;
  ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(restored.data()), size,
                              stream, transfer::TransferOperation::kRestore, &metrics, &error)) << error;
  EXPECT_EQ(restored, source);
}

TEST(CudaTransfer, HostNodeZeroWithoutNuma)
{
  host_numa = -1;
  {
    snapshot::pagebroker::cuda::TransferBuffers buffers({2, chunk_bytes});
    std::string error;
    EXPECT_TRUE(buffers.Initialize(reinterpret_cast<CUcontext>(1), &error)) << error;
  }
  host_numa = 7;
  EXPECT_TRUE(allocations.empty());
  EXPECT_TRUE(reservations.empty());
  EXPECT_TRUE(mappings.empty());
  EXPECT_EQ(events, 0);
}

class AllocationFailureCleanup : public ::testing::TestWithParam<AllocationFailure> {};

TEST_P(AllocationFailureCleanup, ReleasesPartialAllocation)
{
  allocation_failure = GetParam();
  {
    snapshot::pagebroker::cuda::TransferBuffers buffers({2, chunk_bytes});
    std::string error;
    EXPECT_FALSE(buffers.Initialize(reinterpret_cast<CUcontext>(1), &error));
    EXPECT_NE(error.find("allocate CUDA host-NUMA transfer slot"), std::string::npos);
  }
  allocation_failure = AllocationFailure::None;
  EXPECT_TRUE(allocations.empty());
  EXPECT_TRUE(reservations.empty());
  EXPECT_TRUE(mappings.empty());
  EXPECT_EQ(events, 0);
}

INSTANTIATE_TEST_SUITE_P(HostNuma, AllocationFailureCleanup,
    ::testing::Values(AllocationFailure::Create, AllocationFailure::Reserve,
                      AllocationFailure::Map, AllocationFailure::Access, AllocationFailure::Event));
