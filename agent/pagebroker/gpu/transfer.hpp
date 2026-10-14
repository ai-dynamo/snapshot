// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include "cancellation.hpp"
#include <memory>

namespace snapshot::pagebroker::cuda {
struct TransferOptions {
  size_t buffer_count = 32;
  size_t chunk_bytes = 128 * 1024 * 1024;
  bool direct_io = false;
};
// Validate buffer_count * chunk_bytes for all devices. A zero limit means no
// cap. AllocationBytes includes CUDA allocation rounding.
size_t TransferMemoryBytes(const TransferOptions& options, size_t device_count, size_t memory_limit_bytes);

// One persistent ring per device, initialized before the engine is ready.
// The caller serializes access and owns the context, stream, and file. Each file
// holds one contiguous device extent. Both operations drain storage I/O and DMA
// before returning. Open the file with O_DIRECT when direct_io is enabled. Neither
// operation changes file status flags. Cancellation and caller deadlines are
// checked between operations, including after fsync. They do not interrupt NIXL
// waits, CUDA synchronization, or fsync. NIXL waits use a separate request timeout.
// The token must outlive the call. Fatal cleanup retains the calling thread's
// resources while the daemon stops other operations and exits within 30 seconds.
class TransferBuffers {
 public:
  explicit TransferBuffers(TransferOptions options = {});
  ~TransferBuffers();
  // Physical bytes after rounding each slot to this device's allocation
  // granularity. The engine sums these values before enforcing its memory cap.
  static size_t AllocationBytes(CUcontext context, const TransferOptions& options);
  void Initialize(CUcontext context);
  void Checkpoint(int fd, CUdeviceptr device, size_t size, CUstream stream,
                  const Cancellation& cancellation);
  void Restore(int fd, CUdeviceptr device, size_t size, CUstream stream,
               const Cancellation& cancellation);
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}  // namespace snapshot::pagebroker::cuda
