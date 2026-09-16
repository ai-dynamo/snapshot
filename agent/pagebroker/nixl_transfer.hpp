// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "../cmd/cuda-checkpoint-helper/transfer_cancellation.hpp"
#include <memory>
#include <string>
#include <vector>

namespace cuda_checkpoint_transfer {

// PageBroker's POSIX NIXL adapter owns storage requests, not CUDA state.
// Buffers remain registered across files. A slot cannot be reused until Wait
// completes; failed/cancelled requests must drain before its buffer is freed.
class NixlTransfer {
 public:
  NixlTransfer(const std::vector<void*>& buffers, size_t capacity);
  ~NixlTransfer();
  void Open(int descriptor, size_t size);
  void Submit(size_t slot, bool write, size_t offset, size_t size);
  bool Wait(size_t slot, TransferCancellation* cancellation, double* seconds, std::string* error);
  void Close();

 private:
  struct Impl;
  std::unique_ptr<Impl> impl_;
};
}  // namespace cuda_checkpoint_transfer
