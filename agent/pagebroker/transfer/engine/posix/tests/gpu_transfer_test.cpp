// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "file_descriptor.hpp"
#include "gpu/errors.hpp"
#include "transfer/engine/posix/gpu_transfer.hpp"

#include <gtest/gtest.h>
#include <fcntl.h>
#include <unistd.h>
#include <cstdlib>
#include <limits>
#include <stdexcept>
#include <string>
#include <vector>
#include <future>
#include <array>

namespace {
namespace transfer = snapshot::pagebroker::posix;
using snapshot::pagebroker::Cancellation;
constexpr size_t chunk_bytes = 65536;
constexpr size_t data_bytes = 17 * chunk_bytes + 13;

FileDescriptor CreateFile()
{
  char path[] = "/tmp/pagebroker-transfer-XXXXXX";
  FileDescriptor file(mkstemp(path));
  if (file.get() < 0) {
    throw std::runtime_error("create transfer test file");
  }
  unlink(path);
  if (ftruncate(file.get(), data_bytes)) {
    throw std::runtime_error("size transfer test file");
  }
  return file;
}

std::unique_ptr<transfer::TransferPool> MakePool(std::span<const CUcontext> contexts,
                                                 transfer::TransferOptions options)
{
  std::vector<std::unique_ptr<transfer::TransferPoolDevice>> devices;
  std::vector<transfer::TransferPoolDevice*> allocations;
  for (const auto context : contexts) {
    devices.push_back(std::make_unique<transfer::TransferPoolDevice>(context, options));
    allocations.push_back(devices.back().get());
  }
  for (auto& device : devices) device->AllowAccess(allocations);
  return std::make_unique<transfer::TransferPool>(std::move(devices));
}

struct GpuAllocation {
  CUdevice id = 0;
  CUcontext context = nullptr;
  CUstream stream = nullptr;
  CUdeviceptr memory = 0;

  GpuAllocation() = default;
  GpuAllocation(const GpuAllocation&) = delete;
  GpuAllocation& operator=(const GpuAllocation&) = delete;

  void Initialize(int rank)
  {
    using snapshot::pagebroker::gpu::CheckCuda;
    CheckCuda(cuDeviceGet(&id, rank), "get test GPU");
    CheckCuda(cuDevicePrimaryCtxRetain(&context, id), "retain test context");
    CheckCuda(cuCtxSetCurrent(context), "select test context");
    CheckCuda(cuStreamCreate(&stream, CU_STREAM_NON_BLOCKING), "create test stream");
    CheckCuda(cuMemAlloc(&memory, data_bytes), "allocate test memory");
  }

  ~GpuAllocation()
  {
    if (!context) return;
    EXPECT_EQ(cuCtxSetCurrent(context), CUDA_SUCCESS);
    if (stream) { EXPECT_EQ(cuStreamSynchronize(stream), CUDA_SUCCESS); }
    if (memory) { EXPECT_EQ(cuMemFree(memory), CUDA_SUCCESS); }
    if (stream) { EXPECT_EQ(cuStreamDestroy(stream), CUDA_SUCCESS); }
    EXPECT_EQ(cuDevicePrimaryCtxRelease(id), CUDA_SUCCESS);
  }
};

class CudaTransfer : public testing::Test {
 protected:
  void SetUp() override
  {
    ASSERT_EQ(cuInit(0), CUDA_SUCCESS);
    ASSERT_NO_THROW(gpu_.Initialize(0));
    source_.resize(data_bytes);
    for (size_t i = 0; i < data_bytes; ++i) {
      source_[i] = (i * 7 + i / 4096) % 251;
    }
  }

  void RoundTrip(transfer::TransferPool& pool)
  {
    auto file = CreateFile();
    Cancellation cancellation;
    ASSERT_EQ(cuMemcpyHtoD(gpu_.memory, source_.data(), data_bytes), CUDA_SUCCESS);
    ASSERT_NO_THROW(pool.Checkpoint(file.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancellation));
    std::vector<unsigned char> saved(data_bytes);
    ASSERT_EQ(pread(file.get(), saved.data(), data_bytes, 0), static_cast<ssize_t>(data_bytes));
    EXPECT_EQ(saved, source_);
    ASSERT_EQ(cuMemsetD8(gpu_.memory, 0, data_bytes), CUDA_SUCCESS);
    ASSERT_NO_THROW(pool.Restore(file.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancellation));
    std::vector<unsigned char> restored(data_bytes);
    ASSERT_EQ(cuMemcpyDtoH(restored.data(), gpu_.memory, data_bytes), CUDA_SUCCESS);
    EXPECT_EQ(restored, source_);
  }

  GpuAllocation gpu_;
  std::vector<unsigned char> source_;
};

TEST_F(CudaTransfer, ReusesSlotsAcrossFilesAndPartialFinalChunks)
{
  const std::array contexts{gpu_.context};
  auto pool = MakePool(contexts, {4, chunk_bytes});
  for (int iteration = 0; iteration < 3; ++iteration) {
    ASSERT_NO_FATAL_FAILURE(RoundTrip(*pool));
  }
}

TEST_F(CudaTransfer, FailedReadDoesNotPoisonNextFile)
{
  const std::array contexts{gpu_.context};
  auto pool = MakePool(contexts, {2, chunk_bytes});
  auto empty = CreateFile();
  Cancellation cancellation;
  ASSERT_EQ(ftruncate(empty.get(), 0), 0);
  EXPECT_THROW(pool->Restore(empty.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancellation),
               std::runtime_error);
  ASSERT_NO_FATAL_FAILURE(RoundTrip(*pool));
}

TEST_F(CudaTransfer, FailedWriteDoesNotPoisonNextFile)
{
  const std::array contexts{gpu_.context};
  auto pool = MakePool(contexts, {2, chunk_bytes});
  auto file = CreateFile();
  Cancellation cancellation;
  const auto path = "/proc/self/fd/" + std::to_string(file.get());
  FileDescriptor read_only(open(path.c_str(), O_RDONLY | O_CLOEXEC));
  ASSERT_GE(read_only.get(), 0);
  EXPECT_THROW(pool->Checkpoint(read_only.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancellation),
               std::runtime_error);
  ASSERT_NO_FATAL_FAILURE(RoundTrip(*pool));
}

TEST_F(CudaTransfer, CancelledRequestDoesNotPreventLaterTransfers)
{
  const std::array contexts{gpu_.context};
  auto pool = MakePool(contexts, {4, chunk_bytes});
  auto file = CreateFile();
  Cancellation cancelled;
  cancelled.Cancel();
  Cancellation expired(Cancellation::Clock::now());
  EXPECT_THROW(pool->Checkpoint(file.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancelled),
               std::runtime_error);
  EXPECT_THROW(pool->Checkpoint(file.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, expired),
               std::runtime_error);
  ASSERT_NO_FATAL_FAILURE(RoundTrip(*pool));
  EXPECT_THROW(pool->Restore(file.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancelled),
               std::runtime_error);
  EXPECT_THROW(pool->Restore(file.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, expired),
               std::runtime_error);
  ASSERT_NO_FATAL_FAILURE(RoundTrip(*pool));
}

TEST_F(CudaTransfer, PoolRoundTripConcurrentFilesAndRecovery)
{
  const std::array contexts{gpu_.context};
  // 32 slots run two workers, which share the two concurrent files.
  auto owner = MakePool(contexts, {32, chunk_bytes});
  auto& pool = *owner;
  auto first = CreateFile();
  auto second = CreateFile();
  GpuAllocation other;
  ASSERT_NO_THROW(other.Initialize(0));
  Cancellation cancellation;
  std::vector<unsigned char> opposite(source_);
  for (auto& x : opposite) { x ^= 255; }
  ASSERT_EQ(cuMemcpyHtoD(gpu_.memory, source_.data(), data_bytes), CUDA_SUCCESS);
  ASSERT_EQ(cuMemcpyHtoD(other.memory, opposite.data(), data_bytes), CUDA_SUCCESS);
  for (int iteration = 0; iteration < 3; ++iteration) {
    auto write = std::async(std::launch::async, [&] {
      pool.Checkpoint(first.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancellation);
    });
    ASSERT_NO_THROW(pool.Checkpoint(second.get(), other.context, other.memory, data_bytes, other.stream, cancellation));
    ASSERT_NO_THROW(write.get());
    ASSERT_EQ(cuMemsetD8(gpu_.memory, 0, data_bytes), CUDA_SUCCESS);
    ASSERT_EQ(cuMemsetD8(other.memory, 0, data_bytes), CUDA_SUCCESS);
    auto read = std::async(std::launch::async, [&] {
      pool.Restore(first.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancellation);
    });
    ASSERT_NO_THROW(pool.Restore(second.get(), other.context, other.memory, data_bytes, other.stream, cancellation));
    ASSERT_NO_THROW(read.get());
    std::vector<unsigned char> restored(data_bytes);
    ASSERT_EQ(cuMemcpyDtoH(restored.data(), gpu_.memory, data_bytes), CUDA_SUCCESS);
    EXPECT_EQ(restored, source_);
    ASSERT_EQ(cuMemcpyDtoH(restored.data(), other.memory, data_bytes), CUDA_SUCCESS);
    EXPECT_EQ(restored, opposite);
  }
  auto empty = CreateFile();
  ASSERT_EQ(ftruncate(empty.get(), 0), 0);
  EXPECT_THROW(pool.Restore(empty.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancellation), std::runtime_error);
  ASSERT_NO_THROW(pool.Restore(first.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancellation));
  Cancellation cancelled;
  cancelled.Cancel();
  EXPECT_THROW(pool.Restore(first.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancelled), std::runtime_error);
  ASSERT_NO_THROW(pool.Restore(first.get(), gpu_.context, gpu_.memory, data_bytes, gpu_.stream, cancellation));
}

TEST(PooledTransfer, RoutesConcurrentExtentsAcrossAllDevices)
{
  ASSERT_EQ(cuInit(0), CUDA_SUCCESS);
  int count = 0;
  ASSERT_EQ(cuDeviceGetCount(&count), CUDA_SUCCESS);
  ASSERT_GT(count, 0);
  struct Device : GpuAllocation {
    FileDescriptor file = CreateFile();
    std::vector<unsigned char> expected;
  };
  std::vector<std::unique_ptr<Device>> devices;
  std::vector<CUcontext> contexts;
  for (int rank = 0; rank < count; ++rank) {
    auto device = std::make_unique<Device>();
    ASSERT_NO_THROW(device->Initialize(rank));
    device->expected.resize(data_bytes);
    for (size_t i = 0; i < data_bytes; ++i) {
      device->expected[i] = (i * 7 + i / 4096 + rank * 31) % 251;
    }
    ASSERT_EQ(pwrite(device->file.get(), device->expected.data(), data_bytes, 0),
              static_cast<ssize_t>(data_bytes));
    contexts.push_back(device->context);
    devices.push_back(std::move(device));
  }
  // With 2, 4 or 8 GPUs, 25 slots each run 3, 6 or 12 workers, which do not
  // divide the slot budget evenly.
  auto owner = MakePool(contexts, {25, chunk_bytes});
  auto& pool = *owner;
  Cancellation cancellation;
  for (int iteration = 0; iteration < 3; ++iteration) {
    for (const auto& device : devices) {
      ASSERT_EQ(cuCtxSetCurrent(device->context), CUDA_SUCCESS);
      const bool checkpoint = iteration == 1 && device->id % 2 == 0;
      if (checkpoint) {
        ASSERT_EQ(cuMemcpyHtoD(device->memory, device->expected.data(), data_bytes), CUDA_SUCCESS);
        ASSERT_EQ(ftruncate(device->file.get(), 0), 0);
        ASSERT_EQ(ftruncate(device->file.get(), data_bytes), 0);
      } else {
        ASSERT_EQ(cuMemsetD8(device->memory, 0, data_bytes), CUDA_SUCCESS);
      }
    }
    std::vector<std::future<void>> futures;
    futures.reserve(devices.size());
    // Destroy the promise before joining futures if thread creation throws.
    // The broken promise releases workers that already reached the start gate.
    std::promise<void> start;
    const auto ready = start.get_future().share();
    for (const auto& device : devices) {
      const bool checkpoint = iteration == 1 && device->id % 2 == 0;
      futures.push_back(std::async(std::launch::async, [&, p = device.get(), checkpoint, ready] {
        ready.get();
        if (checkpoint) {
          pool.Checkpoint(p->file.get(), p->context, p->memory, data_bytes, p->stream, cancellation);
          return;
        }
        pool.Restore(p->file.get(), p->context, p->memory, data_bytes, p->stream, cancellation);
      }));
    }
    start.set_value();
    for (auto& future : futures) { ASSERT_NO_THROW(future.get()); }
    for (const auto& device : devices) {
      ASSERT_EQ(cuCtxSetCurrent(device->context), CUDA_SUCCESS);
      std::vector<unsigned char> actual(data_bytes);
      ASSERT_EQ(cuMemcpyDtoH(actual.data(), device->memory, data_bytes), CUDA_SUCCESS);
      EXPECT_EQ(actual, device->expected);
      ASSERT_EQ(pread(device->file.get(), actual.data(), data_bytes, 0), static_cast<ssize_t>(data_bytes));
      EXPECT_EQ(actual, device->expected);
    }
  }
}

TEST(TransferOptions, PoolLanesGiveEachWorkerSixteenSlots)
{
  EXPECT_EQ(transfer::PoolLanes(1), 1);
  EXPECT_EQ(transfer::PoolLanes(31), 1);
  EXPECT_EQ(transfer::PoolLanes(47), 2);
  EXPECT_EQ(transfer::PoolLanes(32), 2);
  EXPECT_EQ(transfer::PoolLanes(64), 4);
  EXPECT_EQ(transfer::PoolLanes(256), 16);
  EXPECT_EQ(transfer::PoolLanes(257), 16);
  EXPECT_EQ(transfer::PoolLanes(1024), 16);
}

TEST(TransferOptions, RejectsInvalidCapacity)
{
  EXPECT_THROW(transfer::TransferMemoryBytes({0, chunk_bytes}, 1), std::invalid_argument);
  EXPECT_THROW(transfer::TransferMemoryBytes({1, chunk_bytes + 1}, 1), std::invalid_argument);
  EXPECT_THROW(transfer::TransferMemoryBytes({std::numeric_limits<size_t>::max(), chunk_bytes}, 1),
               std::invalid_argument);
  EXPECT_THROW(transfer::TransferMemoryBytes({2, chunk_bytes}, std::numeric_limits<size_t>::max()),
               std::invalid_argument);
  EXPECT_EQ(transfer::TransferMemoryBytes({2, chunk_bytes}, 2), 4 * chunk_bytes);
}
}  // namespace
