// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include <span>
#include <stdexcept>

namespace snapshot::cuda_checkpoint {

// A failed CustomStorage COMPLETE has unspecified handle/mapping lifetime. The
// owner must retire its process after Abort terminates the target; it cannot
// reuse the operation or assume the driver's imported mappings were released.
class CompletionUncertain : public std::runtime_error {
public:
  using std::runtime_error::runtime_error;
};

// Driver API only: no transfer buffers, storage paths, or broker protocol.
// Initialize CUDA and probe optional CustomStorage support once at startup.
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

// Driver lifecycle only. The caller owns transfer scheduling and must drain
// storage I/O and CUDA copies before Complete or Abort. Returned regions and
// streams must be consumed in this process. Destruction closes the target fd;
// the session owner explicitly completes or aborts first.
class Operation {
public:
  Operation(const CheckpointAPI& api, int pid);
  ~Operation();
  Operation(const Operation&) = delete;
  Operation& operator=(const Operation&) = delete;

  void CheckTarget() const;
  void Lock();
  // Driver-managed operations pass no CustomStorage output to the driver and
  // return nullptr. Complete is then a no-op; Unlock is still explicit.
  const CUcheckpointCustomStorageInfo* Prepare(bool save, std::span<CUcheckpointGpuPair> pairs,
                                               bool custom_storage = true);
  void Complete();
  void Unlock();
  void Abort();

private:
  const CheckpointAPI& api_;
  int pid_;
  int pidfd_;
  bool touched_ = false;
  bool completion_uncertain_ = false;
  CUcheckpointCustomStorageInfo* view_ = nullptr;
};
} // namespace snapshot::cuda_checkpoint
