// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <condition_variable>
#include <memory>
#include <mutex>
#include <variant>

#include "checkpoint_transaction_descriptor.hpp"
#include "restore_transaction_descriptor.hpp"
#include "gpu/engine.hpp"

namespace snapshot::pagebroker {
class Transaction {
 public:
  enum class State { NEW, PREPARING, STAGED, ABORTING, COMMITTED, ABORTED };
  using Descriptor = std::variant<std::monostate, RestoreTransactionDescriptor, CheckpointTransactionDescriptor>;

  struct GpuOperation {
    std::unique_ptr<gpu::Artifact> artifact;
    gpu::Direction direction;
    std::shared_ptr<Cancellation> cancellation;
    bool running = false;
    bool finished = false;
    Response result;
    std::string request_payload;
    std::condition_variable completed;
  };
  // Protected by mutex(), except the atomic cancellation token.
  std::shared_ptr<GpuOperation> gpu_operation;

  // Callers hold mutex() while accessing transaction state.
  std::mutex& mutex();
  State state() const;
  void set_state(State state);
  const Descriptor& descriptor() const;
  void set_descriptor(Descriptor descriptor);
  void clear_descriptor();
  bool retain_terminal();
  bool expired(std::chrono::steady_clock::time_point now, std::chrono::steady_clock::duration lifetime) const;

 private:
  std::mutex mutex_;
  State state_ = State::NEW;
  Descriptor descriptor_;
  std::chrono::steady_clock::time_point staging_started_at_;
  bool terminal_retained_ = false;
};
}  // namespace snapshot::pagebroker
