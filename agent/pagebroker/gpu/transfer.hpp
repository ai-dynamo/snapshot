// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include "cancellation.hpp"
#include <atomic>
#include <memory>
#include <string>

namespace snapshot::pagebroker::cuda {
using TransferCancellation = Cancellation;

enum class TransferOperation { kCheckpoint, kRestore };
struct TransferOptions {
  size_t buffer_count = 32;
  size_t chunk_bytes = 128 * 1024 * 1024;
  bool direct_io = false;
};
// Validate buffer_count * chunk_bytes for all devices. A zero limit means no
// cap. AllocationBytes includes CUDA allocation rounding.
size_t TransferMemoryBytes(const TransferOptions& options, size_t device_count, size_t memory_limit_bytes);

struct TransferControl {
  TransferCancellation* cancellation = nullptr;
};

// One persistent ring per device, initialized before the engine is ready.
// The caller serializes access and owns the context, stream, and file. Each file
// holds one contiguous device extent. Transfer drains storage I/O and DMA before
// returning. Open the file with O_DIRECT when direct_io is enabled. Transfer
// does not change file status flags. On FatalError, keep the file open until process exit.
class TransferBuffers {
 public:
  explicit TransferBuffers(TransferOptions options = {});
  ~TransferBuffers();
  // Physical bytes after rounding each slot to this device's allocation
  // granularity. The engine sums these values before enforcing its memory cap.
  static size_t AllocationBytes(CUcontext context, const TransferOptions& options);
  void Initialize(CUcontext context);
  void Transfer(int fd, CUdeviceptr device, size_t size, CUstream stream,
                TransferOperation operation, TransferControl control = {});
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}  // namespace snapshot::pagebroker::cuda
