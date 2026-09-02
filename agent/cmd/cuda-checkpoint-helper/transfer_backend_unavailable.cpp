/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "transfer_engine.h"

namespace cuda_checkpoint_transfer {

bool TransferBackendAvailable() { return false; }

bool TransferExtent(CUdeviceptr, size_t, CUstream, CUcontext,
                    const StorageLayout &, TransferOperation,
                    const TransferOptions &, TransferCancellation *cancellation,
                    TransferMetrics *metrics, std::string *error) {
  if (metrics != nullptr) {
    *metrics = {};
  }
  if (error != nullptr) {
    *error = "no CustomStorage transfer backend is linked";
  }
  if (cancellation != nullptr) {
    cancellation->Cancel();
  }
  return false;
}

} // namespace cuda_checkpoint_transfer
