// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Exercise the real chunked POSIX pipeline without a GPU. CUDA copies
// use host addresses; this does not qualify driver import/map behavior.
#include "../cmd/cuda-checkpoint-helper/transfer_engine.hpp"
#include "file_descriptor.hpp"
#include "cuda_posix_transfer.hpp"

#include <gtest/gtest.h>
#include <sys/mman.h>
#include <unistd.h>
#include <cstring>
#include <vector>

namespace {
int registrations = 0;
int registration_calls = 0;
bool fail_copy = false;
}

TEST(AllocationTransfer, ReusesPinnedRingAcrossSaveAndLoad)
{
  namespace transfer = cuda_checkpoint_transfer;
  const size_t size = 3 * transfer::kMinimumChunkBytes + 17;
  std::vector<unsigned char> source(size, 37), restored(size);
  FileDescriptor file(memfd_create("reusable-content", MFD_CLOEXEC));
  ASSERT_GE(file.get(), 0);
  ASSERT_EQ(ftruncate(file.get(), size), 0);
  transfer::StorageLayout storage{{{"", size, file.get()}}, {{0, size, 0, 0}}};
  const int before = registration_calls;
  {
    transfer::TransferBuffers buffers({2, transfer::kMinimumChunkBytes});
    transfer::TransferMetrics saved, loaded;
    std::string error;
    const auto context = reinterpret_cast<CUcontext>(1);
    const auto stream = reinterpret_cast<CUstream>(1);
    for (int batch = 0; batch < 3; ++batch) {
      ASSERT_TRUE(buffers.Transfer(reinterpret_cast<CUdeviceptr>(source.data()), size, stream, context,
                                   storage, transfer::TransferOperation::kCheckpoint, nullptr, &saved, &error)) << error;
      ASSERT_TRUE(buffers.Transfer(reinterpret_cast<CUdeviceptr>(restored.data()), size, stream, context,
                                   storage, transfer::TransferOperation::kRestore, nullptr, &loaded, &error)) << error;
      EXPECT_EQ(restored, source);
      EXPECT_EQ(registration_calls - before, 2);
      EXPECT_EQ(registrations, 2);
    }
  }
  EXPECT_EQ(registrations, 0);
}

TEST(AllocationTransfer, AsyncRingDrainsCancellationAndReusesFiles)
{
  namespace transfer = cuda_checkpoint_transfer;
  const size_t size = 17 * transfer::kMinimumChunkBytes + 13;
  std::vector<unsigned char> source(size), restored(size);
  for (size_t i = 0; i < size; ++i)
    source[i] = (i * 7 + i / 4096) % 251;
  transfer::TransferBuffers buffers({4, transfer::kMinimumChunkBytes});
  auto context = reinterpret_cast<CUcontext>(1);
  auto stream = reinterpret_cast<CUstream>(1);
  for (int iteration = 0; iteration < 3; ++iteration) {
    FileDescriptor file(memfd_create("async-ring", MFD_CLOEXEC));
    ASSERT_EQ(ftruncate(file.get(), size), 0);
    transfer::StorageLayout storage{{{"", size, file.get()}}, {{0, size, 0, 0}}};
    transfer::TransferMetrics saved, loaded;
    std::string error;
    ASSERT_TRUE(buffers.Transfer(reinterpret_cast<CUdeviceptr>(source.data()), size, stream, context,
                                storage, transfer::TransferOperation::kCheckpoint, nullptr, &saved, &error)) << error;
    transfer::TransferCancellation cancelled;
    cancelled.Cancel();
    EXPECT_FALSE(buffers.Transfer(reinterpret_cast<CUdeviceptr>(restored.data()), size, stream, context,
                                  storage, transfer::TransferOperation::kRestore, &cancelled, &loaded, &error));
    ASSERT_TRUE(buffers.Transfer(reinterpret_cast<CUdeviceptr>(restored.data()), size, stream, context,
                                storage, transfer::TransferOperation::kRestore, nullptr, &loaded, &error)) << error;
    EXPECT_EQ(restored, source);
  }
}

TEST(AllocationTransfer, BatchRingSpansAllocationBoundariesAndFileOffsets)
{
  namespace transfer = cuda_checkpoint_transfer;
  constexpr size_t size = 65536;
  FileDescriptor file(memfd_create("participant", MFD_CLOEXEC));
  ASSERT_EQ(ftruncate(file.get(), 12 * size), 0);
  std::vector<std::vector<unsigned char>> source(9, std::vector<unsigned char>(size));
  std::vector<std::vector<unsigned char>> restored(9, std::vector<unsigned char>(size));
  std::vector<transfer::AllocationTransfer> save, load;
  for (size_t i = 0; i < source.size(); ++i) {
    std::fill(source[i].begin(), source[i].end(), i + 1);
    save.push_back({reinterpret_cast<CUdeviceptr>(source[i].data()), size, (i + 2) * size});
    load.push_back({reinterpret_cast<CUdeviceptr>(restored[i].data()), size, (i + 2) * size});
  }
  transfer::TransferBuffers buffers({4, transfer::kMinimumChunkBytes});
  transfer::TransferMetrics metrics;
  std::string error;
  auto context = reinterpret_cast<CUcontext>(1);
  auto stream = reinterpret_cast<CUstream>(1);
  ASSERT_TRUE(buffers.TransferBatch(save, file.get(), stream, context,
      transfer::TransferOperation::kCheckpoint, nullptr, &metrics, &error)) << error;
  std::reverse(load.begin(), load.end());
  ASSERT_TRUE(buffers.TransferBatch(load, file.get(), stream, context,
      transfer::TransferOperation::kRestore, nullptr, &metrics, &error)) << error;
  EXPECT_EQ(restored, source);
  load[0].file_offset = 12 * size;
  EXPECT_FALSE(buffers.TransferBatch(load, file.get(), stream, context,
      transfer::TransferOperation::kRestore, nullptr, &metrics, &error));
}

extern "C" {
CUresult CUDAAPI cuGetErrorName(CUresult, const char** name)
{
  *name = "fake CUDA error";
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuCtxSetCurrent(CUcontext) { return CUDA_SUCCESS; }
CUresult CUDAAPI cuMemHostRegister(void*, size_t, unsigned int)
{
  ++registrations;
  ++registration_calls;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuMemHostUnregister(void*)
{
  --registrations;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuEventCreate(CUevent* event, unsigned int)
{
  *event = reinterpret_cast<CUevent>(1);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI cuEventDestroy(CUevent) { return CUDA_SUCCESS; }
CUresult CUDAAPI cuEventSynchronize(CUevent) { return CUDA_SUCCESS; }
CUresult CUDAAPI cuEventRecord(CUevent, CUstream) { return CUDA_SUCCESS; }
CUresult CUDAAPI cuStreamSynchronize(CUstream) { return CUDA_SUCCESS; }
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

TEST(AllocationTransfer, RingRoundTripAndFailedCopyCleanup)
{
  namespace transfer = cuda_checkpoint_transfer;
  const size_t size = 5 * transfer::kMinimumChunkBytes + 37;
  std::vector<unsigned char> source(size), restored(size);
  for (size_t i = 0; i < size; ++i)
    source[i] = i % 251;
  FileDescriptor file(memfd_create("allocation-content", MFD_CLOEXEC));
  ASSERT_GE(file.get(), 0);
  ASSERT_EQ(ftruncate(file.get(), size), 0);
  transfer::StorageLayout storage{{{"", size, file.get()}}, {{0, size, 0, 0}}};
  transfer::TransferOptions options{2, transfer::kMinimumChunkBytes};
  transfer::TransferMetrics saved, loaded;
  std::string error;
  auto context = reinterpret_cast<CUcontext>(1);
  auto stream = reinterpret_cast<CUstream>(1);
  ASSERT_TRUE(transfer::TransferExtent(reinterpret_cast<CUdeviceptr>(source.data()), size, stream, context,
                                      storage, transfer::TransferOperation::kCheckpoint, options,
                                      nullptr, &saved, &error)) << error;
  EXPECT_EQ(saved.bytes, size);
  EXPECT_EQ(registrations, 0);
  ASSERT_TRUE(transfer::TransferExtent(reinterpret_cast<CUdeviceptr>(restored.data()), size, stream, context,
                                      storage, transfer::TransferOperation::kRestore, options,
                                      nullptr, &loaded, &error)) << error;
  EXPECT_EQ(restored, source);
  EXPECT_EQ(registrations, 0);
  fail_copy = true;
  EXPECT_FALSE(transfer::TransferExtent(reinterpret_cast<CUdeviceptr>(source.data()), size, stream, context,
                                       storage, transfer::TransferOperation::kCheckpoint, options,
                                       nullptr, &saved, &error));
  fail_copy = false;
  EXPECT_EQ(registrations, 0);
}
