// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include <memory>
#include <string>

namespace snapshot::pagebroker::cuda {
enum class TransferOperation { kCheckpoint, kRestore };
struct TransferOptions {
  size_t buffer_count = 32;
  size_t chunk_bytes = 128 * 1024 * 1024;
  bool direct_io = false;
};
struct TransferMetrics {
  size_t bytes = 0;
  double setup_seconds = 0;
  double pipeline_seconds = 0;
  double storage_io_seconds = 0;
  double cuda_wait_seconds = 0;
  double fsync_seconds = 0;
  double total_seconds = 0;
};

// One persistent ring per device, initialized before the engine is ready.
// The caller serializes access and owns the context, stream, and file. A file
// contains exactly one contiguous device extent. Transfer drains both storage
// and DMA before returning; it never retains the caller's descriptors.
class TransferBuffers {
 public:
  explicit TransferBuffers(TransferOptions options = {});
  ~TransferBuffers();
  bool Initialize(CUcontext context, std::string* error);
  bool Transfer(int fd, CUdeviceptr device, size_t size, CUstream stream,
                TransferOperation operation, TransferMetrics* metrics, std::string* error);
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}  // namespace snapshot::pagebroker::cuda
