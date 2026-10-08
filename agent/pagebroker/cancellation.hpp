// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <atomic>
#include <chrono>
#include <stdexcept>

#include "fatal_cleanup.hpp"

namespace snapshot::pagebroker {
class Cancellation {
 public:
  using Clock = std::chrono::steady_clock;
  Cancellation() = default;
  explicit Cancellation(Clock::time_point deadline) : deadline_(deadline) {}
  // A batch can stop its other transfers without cancelling its caller.
  // The parent must outlive this token and all work that reads it.
  explicit Cancellation(const Cancellation* parent) : parent_(parent) {}
  void Cancel() { cancelled_.store(true, std::memory_order_relaxed); }
  bool DeadlineExceeded() const { return Clock::now() >= deadline_; }
  bool IsCancelled() const {
    return FatalCleanupPending() || cancelled_.load(std::memory_order_relaxed) || DeadlineExceeded() || (parent_ && parent_->IsCancelled());
  }

  void ThrowIfCancelled() const {
    if (IsCancelled()) {
      throw std::runtime_error(DeadlineExceeded() ? "operation deadline exceeded" : "operation cancelled");
    }
  }

 private:
  std::atomic<bool> cancelled_{false};
  Clock::time_point deadline_ = Clock::time_point::max();
  const Cancellation* parent_ = nullptr;
};
}  // namespace snapshot::pagebroker
