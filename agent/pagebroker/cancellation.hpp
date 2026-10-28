// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <algorithm>
#include <chrono>
#include <memory>
#include <optional>
#include <stdexcept>
#include <stop_token>
#include <utility>

#include "fatal_cleanup.hpp"

namespace snapshot::pagebroker {
class CancellationError : public std::runtime_error {
 public:
  enum class Reason { DEADLINE_EXCEEDED, CANCELLED };

  explicit CancellationError(Reason reason)
      : std::runtime_error(reason == Reason::DEADLINE_EXCEEDED ? "operation deadline exceeded" : "operation cancelled"),
        reason(reason) {}

  const Reason reason;
};

class Cancellation {
 public:
  using Clock = std::chrono::steady_clock;
  Cancellation() = default;
  explicit Cancellation(Clock::time_point deadline) : deadline_(deadline) {}

  void Cancel() noexcept { source_.request_stop(); }
  bool DeadlineExceeded() const noexcept { return Clock::now() >= deadline_; }
  std::optional<CancellationError::Reason> Reason() const noexcept
  {
    if (DeadlineExceeded())
      return CancellationError::Reason::DEADLINE_EXCEEDED;
    if (FatalCleanupPending() || source_.stop_requested())
      return CancellationError::Reason::CANCELLED;
    return std::nullopt;
  }
  bool IsCancelled() const noexcept { return Reason().has_value(); }
  void ThrowIfCancelled() const
  {
    if (const auto reason = Reason())
      throw CancellationError(*reason);
  }

  Clock::time_point Deadline() const noexcept { return deadline_; }
  std::stop_token Token() const noexcept { return source_.get_token(); }

  // A deadline view shares explicit cancellation with its parent scope.
  Cancellation WithDeadline(Clock::time_point deadline) const
  {
    Cancellation view = *this;
    view.deadline_ = std::min(view.deadline_, deadline);
    return view;
  }

  // A child can stop its own work without cancelling the parent.
  static Cancellation ChildOf(const Cancellation& parent) { return Cancellation(parent, nullptr); }
  // Either parent can stop joined work; stopping the join affects neither parent.
  static Cancellation Join(const Cancellation& first, const Cancellation& second)
  {
    return Cancellation(first, &second);
  }

 private:
  struct ForwardStop {
    std::stop_source child;
    void operator()() const noexcept { child.request_stop(); }
  };

  // Each descendant retains all ancestor registrations, even after an
  // intermediate handle goes out of scope. Callbacks own only the child stop
  // state, never a pointer to a handle or to their own registration owner.
  struct ParentLinks {
    std::shared_ptr<const ParentLinks> first_ancestors;
    std::shared_ptr<const ParentLinks> second_ancestors;
    std::stop_callback<ForwardStop> first_stop;
    std::optional<std::stop_callback<ForwardStop>> second_stop;

    ParentLinks(std::stop_source child, const Cancellation& first, const Cancellation* second)
        : first_ancestors(first.parents_), second_ancestors(second ? second->parents_ : nullptr),
          first_stop(first.Token(), ForwardStop{child})
    {
      if (second)
        second_stop.emplace(second->Token(), ForwardStop{std::move(child)});
    }
  };

  Cancellation(const Cancellation& first, const Cancellation* second)
      : deadline_(std::min(first.Deadline(), second ? second->Deadline() : Clock::time_point::max())),
        parents_(std::make_shared<ParentLinks>(source_, first, second)) {}

  std::stop_source source_;
  Clock::time_point deadline_ = Clock::time_point::max();
  std::shared_ptr<const ParentLinks> parents_;
};
using CancellationPtr = std::shared_ptr<Cancellation>;
}  // namespace snapshot::pagebroker
