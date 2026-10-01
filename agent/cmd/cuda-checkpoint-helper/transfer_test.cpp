// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Exercise the real chunked POSIX pipeline without a GPU. CUDA copies
// use host addresses; this does not qualify driver import/map behavior.
#include "file_descriptor.hpp"
#include "transfer.hpp"
#include "content_digest.hpp"

#include <gtest/gtest.h>
#include <sys/mman.h>
#include <unistd.h>
#include <cstdlib>
#include <cstring>
#include <map>
#include <limits>
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
int copy_calls = 0;
snapshot::pagebroker::cuda::TransferCancellation* cancel_on_copy = nullptr;
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
  ++copy_calls;
  std::memcpy(to, reinterpret_cast<const void*>(from), size);
  if (cancel_on_copy) cancel_on_copy->Cancel();
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemcpyHtoDAsync(CUdeviceptr to, const void* from, size_t size, CUstream)
{
  if (fail_copy)
    return CUDA_ERROR_INVALID_VALUE;
  ++copy_calls;
  std::memcpy(reinterpret_cast<void*>(to), from, size);
  if (cancel_on_copy) cancel_on_copy->Cancel();
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
      EXPECT_TRUE(saved.sha256.empty());
      EXPECT_TRUE(loaded.sha256.empty());
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

TEST(CudaTransfer, ChecksumDigestHashesEveryByteInExtentOrderAndDetectsCorruption)
{
  namespace transfer = snapshot::pagebroker::cuda;
  // More than two full rings, with a short tail, detects hashing stale slots
  // or padding bytes rather than precisely the transferred extent.
  const size_t size = 9 * chunk_bytes + 17;
  std::vector<unsigned char> source(size), restored(size);
  for (size_t i = 0; i < size; ++i) source[i] = (i * 13 + i / 7) % 251;
  FileDescriptor file(memfd_create("checksum", MFD_CLOEXEC));
  ASSERT_EQ(ftruncate(file.get(), size), 0);
  transfer::TransferBuffers buffers({4, chunk_bytes});
  transfer::TransferMetrics saved, loaded;
  std::string error;
  ASSERT_TRUE(buffers.Initialize(reinterpret_cast<CUcontext>(1), &error)) << error;
  auto stream = reinterpret_cast<CUstream>(1);
  const transfer::TransferControl control{nullptr, true};
  ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(source.data()), size,
                              stream, transfer::TransferOperation::kCheckpoint, &saved, &error,
                              control)) << error;
  cuda_checkpoint_storage::ContentDigest expected;
  std::string expected_hash;
  ASSERT_TRUE(expected.Update(source.data(), source.size(), &error)) << error;
  ASSERT_TRUE(expected.Finalize(&expected_hash, &error)) << error;
  EXPECT_EQ(saved.sha256, expected_hash);
  ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(restored.data()), size,
                              stream, transfer::TransferOperation::kRestore, &loaded, &error,
                              control)) << error;
  EXPECT_EQ(saved.sha256, loaded.sha256);
  EXPECT_EQ(source, restored);
  const unsigned char corrupted = source[chunk_bytes + 5] ^ 0xff;
  ASSERT_EQ(pwrite(file.get(), &corrupted, 1, chunk_bytes + 5), 1);
  ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(restored.data()), size,
                              stream, transfer::TransferOperation::kRestore, &loaded, &error,
                              control)) << error;
  EXPECT_NE(saved.sha256, loaded.sha256);
}

TEST(CudaTransfer, CancellationDuringSubmissionDrainsBeforeReuse)
{
  namespace transfer = snapshot::pagebroker::cuda;
  const size_t size = 9 * chunk_bytes;
  std::vector<unsigned char> source(size, 83), restored(size);
  FileDescriptor file(memfd_create("cancelled", MFD_CLOEXEC));
  ASSERT_EQ(ftruncate(file.get(), size), 0);
  transfer::TransferBuffers buffers({4, chunk_bytes});
  transfer::TransferMetrics metrics;
  std::string error;
  ASSERT_TRUE(buffers.Initialize(reinterpret_cast<CUcontext>(1), &error)) << error;
  auto stream = reinterpret_cast<CUstream>(1);
  for (auto operation : {transfer::TransferOperation::kCheckpoint, transfer::TransferOperation::kRestore}) {
    transfer::TransferCancellation token;
    const int copies_before = copy_calls;
    const int drains_before = stream_drains;
    cancel_on_copy = &token;
    EXPECT_FALSE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(source.data()), size,
                                 stream, operation, &metrics, &error, {&token, true}));
    cancel_on_copy = nullptr;
    EXPECT_EQ(copy_calls, copies_before + 1);
    EXPECT_EQ(stream_drains, drains_before + 1);
    EXPECT_NE(error.find("cancelled"), std::string::npos);
    EXPECT_TRUE(metrics.sha256.empty());
    ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(source.data()), size,
                                stream, transfer::TransferOperation::kCheckpoint, &metrics, &error)) << error;
    ASSERT_TRUE(buffers.Transfer(file.get(), reinterpret_cast<CUdeviceptr>(restored.data()), size,
                                stream, transfer::TransferOperation::kRestore, &metrics, &error)) << error;
    EXPECT_EQ(source, restored);
  }
}

TEST(CudaTransfer, ExpiredDeadlineSubmitsNoWork)
{
  namespace transfer = snapshot::pagebroker::cuda;
  transfer::TransferCancellation token(transfer::TransferCancellation::Clock::now());
  transfer::TransferBuffers buffers({2, chunk_bytes});
  transfer::TransferMetrics metrics;
  std::string error;
  ASSERT_TRUE(buffers.Initialize(reinterpret_cast<CUcontext>(1), &error)) << error;
  const int before = copy_calls;
  EXPECT_FALSE(buffers.Transfer(-1, 0, chunk_bytes, nullptr, transfer::TransferOperation::kRestore,
                               &metrics, &error, {&token, false}));
  EXPECT_EQ(before, copy_calls);
  EXPECT_NE(error.find("deadline exceeded"), std::string::npos);
}

TEST(CudaTransfer, ValidatesEngineAllocationAndCudaRounding)
{
  namespace transfer = snapshot::pagebroker::cuda;
  size_t total = 0;
  std::string error;
  const size_t per_gpu = size_t{4} * 1024 * 1024 * 1024;
  EXPECT_TRUE(transfer::ValidateTransferMemory({}, 8, 0, &total, &error));
  EXPECT_EQ(total, per_gpu * 8);
  EXPECT_TRUE(transfer::ValidateTransferMemory({}, 2, per_gpu * 2, &total, &error));
  EXPECT_FALSE(transfer::ValidateTransferMemory({}, 2, per_gpu, &total, &error));
  EXPECT_FALSE(transfer::ValidateTransferMemory({0, chunk_bytes}, 1, 0, &total, &error));
  EXPECT_FALSE(transfer::ValidateTransferMemory({1, chunk_bytes + 1}, 1, 0, &total, &error));
  EXPECT_FALSE(transfer::ValidateTransferMemory({std::numeric_limits<size_t>::max(), chunk_bytes},
                                               1, 0, &total, &error));
  EXPECT_FALSE(transfer::ValidateTransferMemory({}, std::numeric_limits<size_t>::max(),
                                               0, &total, &error));
  ASSERT_TRUE(transfer::TransferBuffers::AllocationBytes(reinterpret_cast<CUcontext>(1),
                                                        {4, chunk_bytes}, &total, &error));
  EXPECT_EQ(total, 4 * allocation_granularity);
  EXPECT_FALSE(transfer::TransferBuffers::AllocationBytes(reinterpret_cast<CUcontext>(1),
      {1, std::numeric_limits<size_t>::max() - 4095}, &total, &error));
}

TEST(CudaTransfer, EmptyExtentHasEmptyContentDigest)
{
  namespace transfer = snapshot::pagebroker::cuda;
  transfer::TransferBuffers buffers({2, chunk_bytes});
  transfer::TransferMetrics metrics;
  std::string error;
  FileDescriptor file(memfd_create("empty-checksum", MFD_CLOEXEC));
  ASSERT_TRUE(buffers.Initialize(reinterpret_cast<CUcontext>(1), &error)) << error;
  ASSERT_TRUE(buffers.Transfer(file.get(), 0, 0, nullptr, transfer::TransferOperation::kCheckpoint,
                              &metrics, &error, {nullptr, true})) << error;
  EXPECT_EQ(metrics.sha256, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
}
