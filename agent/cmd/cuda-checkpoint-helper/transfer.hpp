// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include <atomic>
#include <chrono>
#include <memory>
#include <string>

namespace snapshot::pagebroker::cuda {
class TransferCancellation {
public:
  using Clock = std::chrono::steady_clock;

  TransferCancellation() = default;
  explicit TransferCancellation(Clock::time_point deadline)
      : deadline_(deadline) {}

  void Cancel() { cancelled_.store(true, std::memory_order_relaxed); }
  bool DeadlineExceeded() const { return Clock::now() >= deadline_; }
  bool IsCancelled() const {
    return cancelled_.load(std::memory_order_relaxed) || DeadlineExceeded();
  }

private:
  std::atomic<bool> cancelled_{false};
  Clock::time_point deadline_ = Clock::time_point::max();
};

enum class TransferOperation { kCheckpoint, kRestore };
struct TransferOptions {
  size_t buffer_count = 32;
  size_t chunk_bytes = 128 * 1024 * 1024;
  bool direct_io = false;
};
// Validates logical ring capacity. AllocationBytes accounts for CUDA rounding
// when the engine enforces a physical memory cap. Zero disables this logical cap.
bool ValidateTransferMemory(const TransferOptions& options, size_t device_count,
                            size_t memory_limit_bytes, size_t* total_bytes,
                            std::string* error);

struct TransferControl {
  TransferCancellation* cancellation = nullptr;
  bool enable_checksum_digest = false;
};
struct TransferMetrics {
  // Empty unless checksum digests are enabled and the transfer succeeds.
  std::string sha256;
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
  // Physical bytes after rounding each slot to this device's allocation
  // granularity. The engine sums these values before enforcing its memory cap.
  static bool AllocationBytes(CUcontext context, const TransferOptions& options,
                              size_t* bytes, std::string* error);
  bool Initialize(CUcontext context, std::string* error);
  bool Transfer(int fd, CUdeviceptr device, size_t size, CUstream stream,
                TransferOperation operation, TransferMetrics* metrics, std::string* error,
                TransferControl control = {});
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}  // namespace snapshot::pagebroker::cuda
