// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "file_descriptor.hpp"
#include "transfer.hpp"

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
#include <chrono>
#include <thread>

namespace {
namespace transfer = snapshot::pagebroker::cuda;
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

class CudaTransfer : public testing::Test {
 protected:
  void SetUp() override
  {
    ASSERT_EQ(cuInit(0), CUDA_SUCCESS);
    ASSERT_EQ(cuDeviceGet(&device_, 0), CUDA_SUCCESS);
    ASSERT_EQ(cuDevicePrimaryCtxRetain(&context_, device_), CUDA_SUCCESS);
    ASSERT_EQ(cuCtxSetCurrent(context_), CUDA_SUCCESS);
    ASSERT_EQ(cuStreamCreate(&stream_, CU_STREAM_NON_BLOCKING), CUDA_SUCCESS);
    ASSERT_EQ(cuMemAlloc(&data_, data_bytes), CUDA_SUCCESS);
    source_.resize(data_bytes);
    for (size_t i = 0; i < data_bytes; ++i) {
      source_[i] = (i * 7 + i / 4096) % 251;
    }
  }

  void TearDown() override
  {
    if (stream_) {
      EXPECT_EQ(cuStreamSynchronize(stream_), CUDA_SUCCESS);
    }
    if (data_) {
      EXPECT_EQ(cuMemFree(data_), CUDA_SUCCESS);
    }
    if (stream_) {
      EXPECT_EQ(cuStreamDestroy(stream_), CUDA_SUCCESS);
    }
    if (context_) {
      EXPECT_EQ(cuDevicePrimaryCtxRelease(device_), CUDA_SUCCESS);
    }
  }

  void RoundTrip(transfer::TransferBuffers& buffers)
  {
    auto file = CreateFile();
    Cancellation cancellation;
    ASSERT_EQ(cuMemcpyHtoD(data_, source_.data(), data_bytes), CUDA_SUCCESS);
    ASSERT_NO_THROW(buffers.Checkpoint(file.get(), data_, data_bytes, stream_, cancellation));
    std::vector<unsigned char> saved(data_bytes);
    ASSERT_EQ(pread(file.get(), saved.data(), data_bytes, 0), static_cast<ssize_t>(data_bytes));
    EXPECT_EQ(saved, source_);
    ASSERT_EQ(cuMemsetD8(data_, 0, data_bytes), CUDA_SUCCESS);
    ASSERT_NO_THROW(buffers.Restore(file.get(), data_, data_bytes, stream_, cancellation));
    std::vector<unsigned char> restored(data_bytes);
    ASSERT_EQ(cuMemcpyDtoH(restored.data(), data_, data_bytes), CUDA_SUCCESS);
    EXPECT_EQ(restored, source_);
  }

  CUdevice device_ = 0;
  CUcontext context_ = nullptr;
  CUstream stream_ = nullptr;
  CUdeviceptr data_ = 0;
  std::vector<unsigned char> source_;
};

TEST_F(CudaTransfer, ReusesRingAcrossFilesAndPartialFinalChunks)
{
  transfer::TransferBuffers buffers({4, chunk_bytes});
  ASSERT_NO_THROW(buffers.Initialize(context_));
  for (int iteration = 0; iteration < 3; ++iteration) {
    ASSERT_NO_FATAL_FAILURE(RoundTrip(buffers));
  }
}

TEST_F(CudaTransfer, FailedReadDoesNotPoisonNextFile)
{
  transfer::TransferBuffers buffers({2, chunk_bytes});
  ASSERT_NO_THROW(buffers.Initialize(context_));
  auto empty = CreateFile();
  Cancellation cancellation;
  ASSERT_EQ(ftruncate(empty.get(), 0), 0);
  EXPECT_THROW(buffers.Restore(empty.get(), data_, data_bytes, stream_, cancellation),
               std::runtime_error);
  ASSERT_NO_FATAL_FAILURE(RoundTrip(buffers));
}

TEST_F(CudaTransfer, FailedWriteDoesNotPoisonNextFile)
{
  transfer::TransferBuffers buffers({2, chunk_bytes});
  ASSERT_NO_THROW(buffers.Initialize(context_));
  auto file = CreateFile();
  Cancellation cancellation;
  const auto path = "/proc/self/fd/" + std::to_string(file.get());
  FileDescriptor read_only(open(path.c_str(), O_RDONLY | O_CLOEXEC));
  ASSERT_GE(read_only.get(), 0);
  EXPECT_THROW(buffers.Checkpoint(read_only.get(), data_, data_bytes, stream_, cancellation),
               std::runtime_error);
  ASSERT_NO_FATAL_FAILURE(RoundTrip(buffers));
}

TEST_F(CudaTransfer, CancelledRequestDoesNotPreventLaterTransfers)
{
  transfer::TransferBuffers buffers({4, chunk_bytes});
  ASSERT_NO_THROW(buffers.Initialize(context_));
  auto file = CreateFile();
  Cancellation cancelled;
  cancelled.Cancel();
  Cancellation expired(Cancellation::Clock::now());
  EXPECT_THROW(buffers.Checkpoint(file.get(), data_, data_bytes, stream_, cancelled), std::runtime_error);
  EXPECT_THROW(buffers.Checkpoint(file.get(), data_, data_bytes, stream_, expired), std::runtime_error);
  ASSERT_NO_FATAL_FAILURE(RoundTrip(buffers));
  EXPECT_THROW(buffers.Restore(file.get(), data_, data_bytes, stream_, cancelled), std::runtime_error);
  EXPECT_THROW(buffers.Restore(file.get(), data_, data_bytes, stream_, expired), std::runtime_error);
  ASSERT_NO_FATAL_FAILURE(RoundTrip(buffers));
}

TEST_F(CudaTransfer, PoolRoundTripConcurrentFilesAndRecovery)
{
  const std::array contexts{context_};
  transfer::TransferPool pool(contexts, {4, chunk_bytes}, 2);
  auto first = CreateFile();
  auto second = CreateFile();
  CUdeviceptr other = 0;
  CUstream other_stream = nullptr;
  ASSERT_EQ(cuMemAlloc(&other, data_bytes), CUDA_SUCCESS);
  ASSERT_EQ(cuStreamCreate(&other_stream, CU_STREAM_NON_BLOCKING), CUDA_SUCCESS);
  Cancellation cancellation;
  std::vector<unsigned char> opposite(source_);
  for (auto& x : opposite) { x ^= 255; }
  ASSERT_EQ(cuMemcpyHtoD(data_, source_.data(), data_bytes), CUDA_SUCCESS);
  ASSERT_EQ(cuMemcpyHtoD(other, opposite.data(), data_bytes), CUDA_SUCCESS);
  for (int iteration = 0; iteration < 3; ++iteration) {
    auto write = std::async(std::launch::async, [&] {
      pool.Checkpoint(first.get(), context_, data_, data_bytes, stream_, cancellation);
    });
    ASSERT_NO_THROW(pool.Checkpoint(second.get(), context_, other, data_bytes, other_stream, cancellation));
    ASSERT_NO_THROW(write.get());
    ASSERT_EQ(cuMemsetD8(data_, 0, data_bytes), CUDA_SUCCESS);
    ASSERT_EQ(cuMemsetD8(other, 0, data_bytes), CUDA_SUCCESS);
    transfer::TransferFile prepared_first(first.get(), 2);
    transfer::TransferFile prepared_second(second.get(), 2);
    auto read = std::async(std::launch::async, [&] {
      pool.Restore(prepared_first, context_, data_, data_bytes, stream_, cancellation);
    });
    ASSERT_NO_THROW(pool.Restore(prepared_second, context_, other, data_bytes, other_stream, cancellation));
    ASSERT_NO_THROW(read.get());
    std::vector<unsigned char> restored(data_bytes);
    ASSERT_EQ(cuMemcpyDtoH(restored.data(), data_, data_bytes), CUDA_SUCCESS);
    EXPECT_EQ(restored, source_);
    ASSERT_EQ(cuMemcpyDtoH(restored.data(), other, data_bytes), CUDA_SUCCESS);
    EXPECT_EQ(restored, opposite);
  }
  auto empty = CreateFile();
  ASSERT_EQ(ftruncate(empty.get(), 0), 0);
  EXPECT_THROW(pool.Restore(empty.get(), context_, data_, data_bytes, stream_, cancellation), std::runtime_error);
  ASSERT_NO_THROW(pool.Restore(first.get(), context_, data_, data_bytes, stream_, cancellation));
  Cancellation cancelled;
  cancelled.Cancel();
  EXPECT_THROW(pool.Restore(first.get(), context_, data_, data_bytes, stream_, cancelled), std::runtime_error);
  ASSERT_NO_THROW(pool.Restore(first.get(), context_, data_, data_bytes, stream_, cancellation));
  ASSERT_EQ(cuMemFree(other), CUDA_SUCCESS);
  ASSERT_EQ(cuStreamDestroy(other_stream), CUDA_SUCCESS);
}

TEST_F(CudaTransfer, PoolWakesForTransfersAfterIdle)
{
  const std::array contexts{context_};
  transfer::TransferPool pool(contexts, {4, chunk_bytes}, 2);
  auto file = CreateFile();
  ASSERT_EQ(pwrite(file.get(), source_.data(), data_bytes, 0), static_cast<ssize_t>(data_bytes));
  for (int iteration = 0; iteration < 32; ++iteration) {
    // Let file cleanup finish and workers sleep between independent callers.
    std::this_thread::sleep_for(std::chrono::milliseconds{2});
    Cancellation cancellation;
    ASSERT_EQ(cuMemsetD8(data_, 0, data_bytes), CUDA_SUCCESS);
    ASSERT_NO_THROW(pool.Restore(file.get(), context_, data_, data_bytes, stream_, cancellation));
    std::vector<unsigned char> actual(data_bytes);
    ASSERT_EQ(cuMemcpyDtoH(actual.data(), data_, data_bytes), CUDA_SUCCESS);
    EXPECT_EQ(actual, source_);
  }
}

TEST(PooledTransfer, RoutesConcurrentExtentsAcrossAllDevices)
{
  ASSERT_EQ(cuInit(0), CUDA_SUCCESS);
  int count = 0;
  ASSERT_EQ(cuDeviceGetCount(&count), CUDA_SUCCESS);
  ASSERT_GT(count, 0);
  struct Device {
    CUdevice id;
    CUcontext context = nullptr;
    CUstream stream = nullptr;
    CUdeviceptr memory = 0;
    FileDescriptor file = CreateFile();
    std::vector<unsigned char> expected;
    ~Device() {
      if (context) {
        cuCtxSetCurrent(context);
        if (stream) { cuStreamSynchronize(stream); }
        if (memory) { cuMemFree(memory); }
        if (stream) { cuStreamDestroy(stream); }
        cuDevicePrimaryCtxRelease(id);
      }
    }
  };
  std::vector<std::unique_ptr<Device>> devices;
  std::vector<CUcontext> contexts;
  for (int rank = 0; rank < count; ++rank) {
    auto device = std::make_unique<Device>();
    ASSERT_EQ(cuDeviceGet(&device->id, rank), CUDA_SUCCESS);
    ASSERT_EQ(cuDevicePrimaryCtxRetain(&device->context, device->id), CUDA_SUCCESS);
    ASSERT_EQ(cuCtxSetCurrent(device->context), CUDA_SUCCESS);
    ASSERT_EQ(cuStreamCreate(&device->stream, CU_STREAM_NON_BLOCKING), CUDA_SUCCESS);
    ASSERT_EQ(cuMemAlloc(&device->memory, data_bytes), CUDA_SUCCESS);
    device->expected.resize(data_bytes);
    for (size_t i = 0; i < data_bytes; ++i) {
      device->expected[i] = (i * 7 + i / 4096 + rank * 31) % 251;
    }
    ASSERT_EQ(pwrite(device->file.get(), device->expected.data(), data_bytes, 0),
              static_cast<ssize_t>(data_bytes));
    contexts.push_back(device->context);
    devices.push_back(std::move(device));
  }
  // Uneven slot distribution also exercises arbitrary lane counts.
  transfer::TransferPool pool(contexts, {4, chunk_bytes}, count + 1);
  Cancellation cancellation;
  for (int iteration = 0; iteration < 3; ++iteration) {
    std::vector<std::future<void>> futures;
    for (const auto& device : devices) {
      ASSERT_EQ(cuCtxSetCurrent(device->context), CUDA_SUCCESS);
      ASSERT_EQ(cuMemsetD8(device->memory, 0, data_bytes), CUDA_SUCCESS);
      futures.push_back(std::async(std::launch::async, [&, p = device.get()] {
        pool.Restore(p->file.get(), p->context, p->memory, data_bytes, p->stream, cancellation);
      }));
    }
    for (auto& future : futures) { ASSERT_NO_THROW(future.get()); }
    for (const auto& device : devices) {
      ASSERT_EQ(cuCtxSetCurrent(device->context), CUDA_SUCCESS);
      std::vector<unsigned char> actual(data_bytes);
      ASSERT_EQ(cuMemcpyDtoH(actual.data(), device->memory, data_bytes), CUDA_SUCCESS);
      EXPECT_EQ(actual, device->expected);
    }
  }
}

TEST(TransferOptions, RejectsInvalidCapacityAndMemoryLimit)
{
  EXPECT_THROW(transfer::TransferMemoryBytes({0, chunk_bytes}, 1, 0), std::invalid_argument);
  EXPECT_THROW(transfer::TransferMemoryBytes({1, chunk_bytes + 1}, 1, 0), std::invalid_argument);
  EXPECT_THROW(transfer::TransferMemoryBytes({std::numeric_limits<size_t>::max(), chunk_bytes}, 1, 0),
               std::invalid_argument);
  EXPECT_THROW(transfer::TransferMemoryBytes({2, chunk_bytes}, std::numeric_limits<size_t>::max(), 0),
               std::invalid_argument);
  EXPECT_THROW(transfer::TransferMemoryBytes({2, chunk_bytes}, 2, 3 * chunk_bytes), std::invalid_argument);
  EXPECT_EQ(transfer::TransferMemoryBytes({2, chunk_bytes}, 2, 0), 4 * chunk_bytes);
}
}  // namespace
