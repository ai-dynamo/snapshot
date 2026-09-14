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

constexpr size_t kMaximumConcurrentTransferJobs = 64;

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
  size_t peak_pinned_bytes = 0;
  size_t maximum_concurrent_jobs = 0;
  size_t waves = 0;
  std::string error;
};

// Return the number of extent workers that may allocate transfer buffers at
// once. Total accepted work is not capped by this value: TransferBatch drains
// it in deterministic waves so a <=64-target restore is not rejected merely
// because it contains many device extents or its aggregate configured buffers
// exceed the process pinned-memory budget.
bool TransferConcurrencyLimit(const TransferOptions &options, size_t *jobs,
                              size_t *peak_pinned_bytes,
                              std::string *error);

// TransferBatch owns the per-extent worker lifetime and cooperative sibling
// cancellation. At most kMaximumConcurrentTransferJobs and 2 GiB of configured
// pinned buffers are live at once. The caller remains responsible for CUDA
// operation completion.
bool TransferBatch(const std::vector<ScheduledTransfer> &jobs,
                   TransferOperation operation, const TransferOptions &options,
                   std::chrono::steady_clock::time_point deadline,
                   TransferBatchResult *result,
                   TransferCancellation *external_cancellation = nullptr);

} // namespace cuda_checkpoint_transfer
