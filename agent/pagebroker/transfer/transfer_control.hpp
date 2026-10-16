// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <stdexcept>
#include <stop_token>

namespace snapshot::pagebroker {
// Distinguishes deadline expiry and cancellation from storage failures.
class TransferInterrupted : public std::runtime_error {
 public:
  // A deadline takes precedence when both stop conditions are observed.
  enum class Reason { DEADLINE_EXCEEDED, CANCELLED };

  // Records the stop reason and selects its diagnostic message.
  explicit TransferInterrupted(Reason reason)
      : std::runtime_error(reason == Reason::DEADLINE_EXCEEDED ? "transfer deadline exceeded" : "transfer cancelled"),
        reason(reason) {}

  const Reason reason;
};

// Transfer work calls Check() to observe cancellation and deadlines.
// Default construction leaves both disabled.
struct TransferControl {
  using Clock = std::chrono::steady_clock;
  Clock::time_point deadline = Clock::time_point::max();
  std::stop_token cancellation;

  // Throws on interruption; does not stop or drain native I/O.
  void Check() const
  {
    if (Clock::now() >= deadline)
      throw TransferInterrupted(TransferInterrupted::Reason::DEADLINE_EXCEEDED);
    if (cancellation.stop_requested())
      throw TransferInterrupted(TransferInterrupted::Reason::CANCELLED);
  }
};
}  // namespace snapshot::pagebroker
