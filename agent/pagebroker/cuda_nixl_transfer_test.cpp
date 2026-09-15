/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "transfer_engine.h"

#include <cuda.h>
#include <fcntl.h>
#include <gtest/gtest.h>
#include <sys/stat.h>
#include <unistd.h>

#include <algorithm>
#include <cstdlib>
#include <cstdint>
#include <cstring>
#include <filesystem>
#include <stdexcept>
#include <string>
#include <vector>

struct CUevent_st {};

namespace {

namespace fs = std::filesystem;
namespace transfer = cuda_checkpoint_transfer;

void *DeviceAddress(CUdeviceptr address) {
  return reinterpret_cast<void *>(static_cast<uintptr_t>(address));
}

class TemporaryDirectory {
public:
  TemporaryDirectory() {
    char path[] = "/tmp/pagebroker-nixl-test-XXXXXX";
    char *created = mkdtemp(path);
    if (created == nullptr) {
      throw std::runtime_error("mkdtemp failed");
    }
    path_ = created;
  }

  ~TemporaryDirectory() {
    std::error_code ignored;
    fs::remove_all(path_, ignored);
  }

  const fs::path &path() const { return path_; }

private:
  fs::path path_;
};

transfer::StorageLayout AbsoluteLayout(const fs::path &path, size_t size) {
  return {
      .files = {{.path = path, .size = size}},
      .ranges = {{.logical_offset = 0,
                  .size = size,
                  .file_index = 0,
                  .file_offset = 0}},
  };
}

transfer::StorageLayout PinnedLayout(int fd, const struct stat &identity,
                                     size_t size) {
  return {
      .files = {{.path = "device-0000.bin",
                 .size = size,
                 .descriptor_fd = fd,
                 .device = static_cast<uint64_t>(identity.st_dev),
                 .inode = static_cast<uint64_t>(identity.st_ino)}},
      .ranges = {{.logical_offset = 0,
                  .size = size,
                  .file_index = 0,
                  .file_offset = 0}},
  };
}

} // namespace

CUresult CUDAAPI cuCtxSetCurrent(CUcontext) { return CUDA_SUCCESS; }

CUresult CUDAAPI cuMemHostRegister(void *, size_t, unsigned int) {
  return CUDA_SUCCESS;
}

CUresult CUDAAPI cuMemHostUnregister(void *) { return CUDA_SUCCESS; }

CUresult CUDAAPI cuEventCreate(CUevent *event, unsigned int) {
  *event = new CUevent_st;
  return CUDA_SUCCESS;
}

CUresult CUDAAPI cuEventDestroy(CUevent event) {
  delete event;
  return CUDA_SUCCESS;
}

CUresult CUDAAPI cuEventRecord(CUevent, CUstream) { return CUDA_SUCCESS; }

CUresult CUDAAPI cuEventSynchronize(CUevent) { return CUDA_SUCCESS; }

CUresult CUDAAPI cuStreamSynchronize(CUstream) { return CUDA_SUCCESS; }

CUresult CUDAAPI cuMemcpyDtoHAsync(void *destination, CUdeviceptr source,
                                   size_t bytes, CUstream) {
  std::memcpy(destination, DeviceAddress(source), bytes);
  return CUDA_SUCCESS;
}

CUresult CUDAAPI cuMemcpyHtoDAsync(CUdeviceptr destination, const void *source,
                                   size_t bytes, CUstream) {
  std::memcpy(DeviceAddress(destination), source, bytes);
  return CUDA_SUCCESS;
}

CUresult CUDAAPI cuGetErrorName(CUresult, const char **name) {
  *name = "CUDA_ERROR_TEST";
  return CUDA_SUCCESS;
}

CUresult CUDAAPI cuGetErrorString(CUresult, const char **message) {
  *message = "test CUDA error";
  return CUDA_SUCCESS;
}

namespace {

TEST(CudaNixlTransfer, RoundTripsPinnedCarrierAndDigest) {
  TemporaryDirectory temporary;
  const fs::path carrier = temporary.path() / "device-0000.bin";
  std::vector<unsigned char> expected(2 * transfer::kMinimumChunkBytes + 123);
  for (size_t index = 0; index < expected.size(); ++index) {
    expected[index] = static_cast<unsigned char>((index * 131U + 17U) & 0xffU);
  }

  const transfer::TransferOptions options{
      .buffer_count = 2,
      .chunk_bytes = transfer::kMinimumChunkBytes,
  };
  transfer::TransferCancellation checkpoint_cancellation;
  transfer::TransferMetrics checkpoint_metrics;
  std::string error;
  ASSERT_TRUE(transfer::TransferExtent(
      reinterpret_cast<CUdeviceptr>(expected.data()), expected.size(),
      reinterpret_cast<CUstream>(1), reinterpret_cast<CUcontext>(1),
      AbsoluteLayout(carrier, expected.size()),
      transfer::TransferOperation::kCheckpoint, options,
      &checkpoint_cancellation, &checkpoint_metrics, &error))
      << error;
  EXPECT_EQ(checkpoint_metrics.bytes, expected.size());
  ASSERT_EQ(checkpoint_metrics.sha256.size(), 64u);

  const int descriptor =
      open(carrier.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
  ASSERT_GE(descriptor, 0);
  struct stat identity {};
  ASSERT_EQ(fstat(descriptor, &identity), 0);
  ASSERT_TRUE(fs::remove(carrier));

  std::vector<unsigned char> restored(expected.size(), 0);
  transfer::TransferCancellation restore_cancellation;
  transfer::TransferMetrics restore_metrics;
  ASSERT_TRUE(transfer::TransferExtent(
      reinterpret_cast<CUdeviceptr>(restored.data()), restored.size(),
      reinterpret_cast<CUstream>(1), reinterpret_cast<CUcontext>(1),
      PinnedLayout(descriptor, identity, restored.size()),
      transfer::TransferOperation::kRestore, options, &restore_cancellation,
      &restore_metrics, &error))
      << error;
  EXPECT_EQ(restored, expected);
  EXPECT_EQ(restore_metrics.bytes, expected.size());
  EXPECT_EQ(restore_metrics.sha256, checkpoint_metrics.sha256);
  EXPECT_EQ(close(descriptor), 0);
}

} // namespace
