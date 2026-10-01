// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <stdexcept>
#include <stop_token>

namespace snapshot::pagebroker {
// Reports a cooperative stop separately from storage and integrity failures so
// callers can distinguish an expired deadline from an explicit cancellation.
class TransferInterrupted : public std::runtime_error {
 public:
  // A deadline takes precedence when both stop conditions are observed.
  enum class Reason { DEADLINE_EXCEEDED, CANCELLED };

  // Retains the reason for error translation and supplies a diagnostic message.
  explicit TransferInterrupted(Reason reason)
      : std::runtime_error(reason == Reason::DEADLINE_EXCEEDED ? "transfer deadline exceeded" : "transfer cancelled"),
        reason(reason) {}

  const Reason reason;
};

// Carries a deadline and a shared cancellation signal through transfer work.
// The owner requests cancellation through its stop_source; operations must call
// Check to observe it. Default construction imposes neither stop condition.
struct TransferControl {
  using Clock = std::chrono::steady_clock;
  Clock::time_point deadline = Clock::time_point::max();
  std::stop_token cancellation;

  // Throws TransferInterrupted when the deadline has passed or a stop was
  // requested. This check does not itself stop or drain native I/O.
  void Check() const
  {
    if (Clock::now() >= deadline)
      throw TransferInterrupted(TransferInterrupted::Reason::DEADLINE_EXCEEDED);
    if (cancellation.stop_requested())
      throw TransferInterrupted(TransferInterrupted::Reason::CANCELLED);
  }
};
}  // namespace snapshot::pagebroker
