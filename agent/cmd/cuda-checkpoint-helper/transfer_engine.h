/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cuda.h>

#include <cstddef>
#include <string>
#include <vector>

#include "transfer_cancellation.h"
#include "transfer_config.h"

namespace cuda_checkpoint_transfer {

struct StorageFileMetrics {
  size_t bytes = 0;
  double storage_seconds = 0.0;
  double fsync_seconds = 0.0;
};

struct TransferMetrics {
  size_t bytes = 0;
  double setup_seconds = 0.0;
  double pipeline_seconds = 0.0;
  double storage_seconds = 0.0;
  double cuda_wait_seconds = 0.0;
  double fsync_seconds = 0.0;
  double cleanup_seconds = 0.0;
  double total_seconds = 0.0;
  std::string sha256;
  std::vector<StorageFileMetrics> files;
};

// TransferBackendAvailable reports whether this helper binary was linked with
// a transfer adapter that can service CustomStorage extents.
bool TransferBackendAvailable();

// cancellation is a shared mutable operation token. A backend may cancel it
// when this extent fails, including when the backend is unavailable, so sibling
// transfers observe the failure and stop. Callers retain ownership and must
// keep the token alive for every transfer that shares it. Pass nullptr when no
// shared cancellation token is needed.
bool TransferExtent(CUdeviceptr device_ptr, size_t extent_size, CUstream stream,
                    CUcontext context, const StorageLayout &storage,
                    TransferOperation operation, const TransferOptions &options,
                    TransferCancellation *cancellation,
                    TransferMetrics *metrics, std::string *error);

} // namespace cuda_checkpoint_transfer
