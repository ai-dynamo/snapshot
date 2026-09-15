/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <atomic>
#include <chrono>

namespace cuda_checkpoint_transfer {

class TransferCancellation {
public:
  using Clock = std::chrono::steady_clock;

  TransferCancellation() = default;
  explicit TransferCancellation(Clock::time_point deadline)
      : deadline_(deadline) {}

  void Cancel() { cancelled_.store(true, std::memory_order_relaxed); }
  bool DeadlineExceeded() const { return Clock::now() >= deadline_; }
  bool IsCancelled() const {
    return cancelled_.load(std::memory_order_relaxed) || DeadlineExceeded();
  }

private:
  std::atomic<bool> cancelled_{false};
  Clock::time_point deadline_ = Clock::time_point::max();
};

} // namespace cuda_checkpoint_transfer
