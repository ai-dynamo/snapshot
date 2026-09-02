/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cuda.h>

#include <chrono>
#include <cstddef>
#include <string>
#include <vector>

#include "transfer_engine.h"

namespace cuda_checkpoint_transfer {

struct ScheduledTransfer {
  CUdeviceptr device_ptr = 0;
  size_t extent_size = 0;
  CUstream stream = nullptr;
  CUcontext context = nullptr;
  StorageLayout storage;
  size_t device_index = 0;
};

struct TransferBatchResult {
  std::vector<TransferMetrics> metrics;
  double orchestration_seconds = 0.0;
  std::string error;
};

// TransferBatch owns the per-extent worker lifetime and cooperative sibling
// cancellation. The caller remains responsible for CUDA operation completion.
bool TransferBatch(const std::vector<ScheduledTransfer> &jobs,
                   TransferOperation operation, const TransferOptions &options,
                   std::chrono::steady_clock::time_point deadline,
                   TransferBatchResult *result);

} // namespace cuda_checkpoint_transfer
