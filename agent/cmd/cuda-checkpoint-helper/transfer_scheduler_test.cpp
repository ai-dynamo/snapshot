/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "transfer_scheduler.h"

#include <cassert>
#include <chrono>
#include <string>

int main() {
  using namespace std::chrono_literals;
  using cuda_checkpoint_transfer::TransferBatch;
  using cuda_checkpoint_transfer::TransferBatchResult;
  using cuda_checkpoint_transfer::TransferCancellation;
  using cuda_checkpoint_transfer::TransferOperation;
  using cuda_checkpoint_transfer::ScheduledTransfer;

  TransferBatchResult empty_result;
  assert(TransferBatch({}, TransferOperation::kCheckpoint, {},
                       TransferCancellation::Clock::now() + 1h,
                       &empty_result));
  assert(empty_result.metrics.empty());
  assert(empty_result.error.empty());
  assert(!TransferBatch({}, TransferOperation::kCheckpoint, {},
                        TransferCancellation::Clock::now() + 1h, nullptr));

  ScheduledTransfer unavailable;
  unavailable.device_index = 7;
  TransferBatchResult unavailable_result;
  assert(!TransferBatch({unavailable}, TransferOperation::kCheckpoint, {},
                        TransferCancellation::Clock::now() + 1h,
                        &unavailable_result));
  assert(unavailable_result.metrics.size() == 1);
  assert(unavailable_result.error ==
         "custom storage transfer failed for device index 7: no "
         "CustomStorage transfer backend is linked");
  return 0;
}
