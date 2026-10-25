// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cuda.h>
#include "errors.hpp"
#include "file_descriptor.hpp"
#include <span>

namespace snapshot::pagebroker::gpu::driver {

// Validate the supplied pidfd in the broker's PID namespace without opening a
// new process handle. CUDA still accepts numeric PIDs, so its calls are not atomic
// with this check and do not reserve the PID against reuse.
void ValidateTargetDescriptor(int pid, int pidfd);

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
  Operation(const CheckpointAPI& api, int pid, FileDescriptor pidfd);
  Operation(const Operation&) = delete;
  Operation& operator=(const Operation&) = delete;

  void CheckTarget() const;
  void Lock();
  const CUcheckpointCustomStorageInfo* PrepareCheckpoint();
  const CUcheckpointCustomStorageInfo* PrepareRestore(std::span<CUcheckpointGpuPair> pairs);
  void Complete();
  void Unlock();
  // Call after all participants complete. Unlock targets that only reached
  // Lock. Terminate targets whose preparation may have changed CUDA state.
  void Abort();

private:
  const CUcheckpointCustomStorageInfo* StorageInfo() const;
  bool Exited() const;
  void WaitForTargetExit() const;

  const CheckpointAPI& api_;
  int pid_;
  FileDescriptor pidfd_;
  bool locked_ = false;
  bool prepare_started_ = false;
  bool completion_failed_ = false;
  CUcheckpointCustomStorageInfo* view_ = nullptr;
};
} // namespace snapshot::pagebroker::gpu::driver
