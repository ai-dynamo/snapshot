// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include "errors.hpp"
#include <span>

namespace snapshot::pagebroker::gpu::driver {

// Initialize CUDA and check CustomStorage support once at startup.
class CheckpointAPI {
public:
  CheckpointAPI();
  bool
  SupportsCustomStorage() const
  {
    return complete_ != nullptr;
  }
  void RequireCustomStorage() const;

private:
  friend class Operation;
  decltype(&cuCheckpointOperationComplete) complete_ = nullptr;
};

// The caller must drain storage I/O and CUDA copies before Complete. Returned
// regions and streams can only be used in this process. Destruction closes the
// target fd. The batch owner completes all participants before killing any.
class Operation {
public:
  Operation(const CheckpointAPI& api, int pid);
  ~Operation();
  Operation(const Operation&) = delete;
  Operation& operator=(const Operation&) = delete;

  void CheckTarget() const;
  void Lock();
  const CUcheckpointCustomStorageInfo* Prepare(bool save, std::span<CUcheckpointGpuPair> pairs);
  void Complete();
  void Unlock();
  // Does not access the CustomStorage handle, even after Complete fails.
  void Terminate();

private:
  const CheckpointAPI& api_;
  int pid_;
  int pidfd_;
  bool touched_ = false;
  bool completion_failed_ = false;
  CUcheckpointCustomStorageInfo* view_ = nullptr;
};
} // namespace snapshot::pagebroker::gpu::driver
