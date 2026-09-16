// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "../cmd/cuda-checkpoint-helper/transfer_engine.hpp"
#include <memory>

namespace cuda_checkpoint_transfer {
// A worker keeps one bounded ring per device. Every successful transfer drains
// its DMA before returning; an uncertain outcome requires worker termination.
class TransferBuffers {
 public:
  explicit TransferBuffers(TransferOptions options);
  ~TransferBuffers();
  bool Transfer(CUdeviceptr device, size_t size, CUstream stream, CUcontext context,
                const StorageLayout& storage, TransferOperation operation,
                TransferCancellation* cancellation, TransferMetrics* metrics, std::string* error);
 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}  // namespace cuda_checkpoint_transfer
