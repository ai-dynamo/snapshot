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
