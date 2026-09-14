/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#include "transfer_scheduler.h"

#include <cassert>
#include <chrono>
#include <string>
#include <vector>

int main() {
  using namespace std::chrono_literals;
  using cuda_checkpoint_transfer::TransferBatch;
  using cuda_checkpoint_transfer::TransferBatchResult;
  using cuda_checkpoint_transfer::TransferConcurrencyLimit;
  using cuda_checkpoint_transfer::TransferCancellation;
  using cuda_checkpoint_transfer::kMaximumConcurrentTransferJobs;
  using cuda_checkpoint_transfer::TransferOperation;
  using cuda_checkpoint_transfer::ScheduledTransfer;

  TransferBatchResult empty_result;
  assert(TransferBatch({}, TransferOperation::kCheckpoint, {},
                       TransferCancellation::Clock::now() + 1h,
                       &empty_result));
  assert(empty_result.metrics.empty());
  assert(empty_result.error.empty());
  TransferCancellation cancelled;
  cancelled.Cancel();
  assert(!TransferBatch({}, TransferOperation::kCheckpoint, {},
                        TransferCancellation::Clock::now() + 1h,
                        &empty_result, &cancelled));
  assert(empty_result.error == "custom storage transfer cancelled");
  assert(!TransferBatch({}, TransferOperation::kCheckpoint, {},
                        TransferCancellation::Clock::now() + 1h, nullptr));

  ScheduledTransfer unavailable;
  unavailable.device_index = 7;
  TransferBatchResult unavailable_result;
  TransferCancellation external_cancellation(
      TransferCancellation::Clock::now() + 1h);
  assert(!TransferBatch({unavailable}, TransferOperation::kCheckpoint, {},
                        TransferCancellation::Clock::now() + 1h,
                        &unavailable_result, &external_cancellation));
  assert(external_cancellation.IsCancelled());
  assert(unavailable_result.metrics.size() == 1);
  assert(unavailable_result.error ==
         "custom storage transfer failed for device index 7: no "
         "CustomStorage transfer backend is linked");

  size_t concurrency = 0;
  size_t peak_pinned_bytes = 0;
  std::string limit_error;
  assert(TransferConcurrencyLimit({}, &concurrency, &peak_pinned_bytes,
                                  &limit_error));
  assert(concurrency == 32);
  assert(peak_pinned_bytes ==
         cuda_checkpoint_transfer::kMaximumPinnedBytesPerOperation);

  TransferBatchResult many_extents_result;
  assert(!TransferBatch(
      std::vector<ScheduledTransfer>(kMaximumConcurrentTransferJobs + 1),
      TransferOperation::kRestore, {},
      TransferCancellation::Clock::now() + 1h, &many_extents_result));
  // The unavailable fake backend fails the first wave, but admission and
  // scheduling cover all 65 extents instead of rejecting the accepted batch.
  assert(many_extents_result.metrics.size() ==
         kMaximumConcurrentTransferJobs + 1);
  assert(many_extents_result.maximum_concurrent_jobs == 32);
  assert(many_extents_result.waves == 3);
  assert(many_extents_result.error.find("64-worker limit") ==
         std::string::npos);
  return 0;
}
