// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "../cmd/cuda-checkpoint-helper/transfer_engine.hpp"
#include <memory>

namespace cuda_checkpoint_transfer {
struct AllocationTransfer {
  CUdeviceptr address;
  size_t size;
  size_t file_offset;
};
// A worker keeps one bounded ring per device. Every successful transfer drains
// its DMA before returning; an uncertain outcome requires worker termination.
class TransferBuffers {
 public:
  explicit TransferBuffers(TransferOptions options);
  ~TransferBuffers();
  bool Initialize(CUcontext context, std::string* error);
  bool Transfer(CUdeviceptr device, size_t size, CUstream stream, CUcontext context,
                const StorageLayout& storage, TransferOperation operation,
                TransferCancellation* cancellation, TransferMetrics* metrics, std::string* error,
                bool sync_file = true);
  bool TransferBatch(const std::vector<AllocationTransfer>& allocations, int content_fd,
                     CUstream stream, CUcontext context, TransferOperation operation,
                     TransferCancellation* cancellation, TransferMetrics* metrics, std::string* error);
 private:
  bool TransferChunks(const std::vector<TransferChunk>& chunks, size_t size,
                      CUstream stream, CUcontext context, const StorageLayout& storage,
                      TransferOperation operation, TransferCancellation* cancellation,
                      TransferMetrics* metrics, std::string* error, bool sync_file);
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}  // namespace cuda_checkpoint_transfer
