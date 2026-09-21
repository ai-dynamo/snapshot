// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include <span>

namespace snapshot::cuda_checkpoint {

// Initialize once in the persistent worker, before accepting operations.
class CustomStorage {
 public:
  CustomStorage();

 private:
  friend class Operation;
  decltype(&cuCheckpointOperationComplete) complete_;
};

// Driver lifecycle only. The caller owns transfer scheduling and must drain
// storage I/O and CUDA copies before Complete or Abort. Returned regions and
// streams must be consumed in this process. Destruction closes the target fd;
// the session owner explicitly completes or aborts first.
class Operation {
 public:
  Operation(const CustomStorage& api, int pid);
  ~Operation();
  Operation(const Operation&) = delete;
  Operation& operator=(const Operation&) = delete;

  void CheckTarget() const;
  void Lock();
  const CUcheckpointCustomStorageInfo& Prepare(bool save, std::span<CUcheckpointGpuPair> pairs);
  void Complete();
  void Unlock();
  void Abort();

 private:
  const CustomStorage& api_;
  int pid_;
  int pidfd_;
  bool touched_ = false;
  CUcheckpointCustomStorageInfo* view_ = nullptr;
};
}  // namespace snapshot::cuda_checkpoint
