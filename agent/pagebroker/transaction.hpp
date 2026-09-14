// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <cstdint>
#include <mutex>
#include <memory>
#include <optional>
#include <variant>

#include "checkpoint_transaction_descriptor.hpp"
#include "cuda_engine.hpp"
#include "restore_transaction_descriptor.hpp"

namespace snapshot::pagebroker {
class Transaction {
 public:
  enum class State {
    NEW,
    PREPARING,
    STAGED,
    CHECKPOINT_ADMITTED,
    RESTORE_ADMITTED,
    CUDA_STARTED,
    CUDA_COMPLETE,
    PUBLISHED_CLEANUP_PENDING,
    COMMITTED,
    ABORTED
  };
  using Descriptor = std::variant<std::monostate, RestoreTransactionDescriptor, CheckpointTransactionDescriptor>;

  // Callers hold mutex() while accessing transaction state.
  std::mutex& mutex();
  State state() const;
  void set_state(State state);
  const Descriptor& descriptor() const;
  Descriptor& descriptor();
  void set_descriptor(Descriptor descriptor);
  void clear_descriptor();
  void set_restore_admission(
      std::unique_ptr<RestoreAdmission> admission, CudaStorageBackend backend,
      size_t target_count);
  std::unique_ptr<RestoreAdmission> take_restore_admission();
  bool has_restore_admission() const;
  std::optional<CudaStorageBackend> restore_backend() const;
  std::optional<size_t> restore_target_count() const;
  void clear_restore_admission();
  void set_checkpoint_admission(
      std::unique_ptr<CheckpointAdmission> admission,
      CudaStorageBackend backend, size_t target_count);
  std::unique_ptr<CheckpointAdmission> take_checkpoint_admission();
  bool has_checkpoint_admission() const;
  std::optional<CudaStorageBackend> checkpoint_backend() const;
  std::optional<size_t> checkpoint_target_count() const;
  void clear_checkpoint_admission();
  uintmax_t staging_reservation_bytes() const;
  void set_staging_reservation_bytes(uintmax_t bytes);
  size_t direct_restore_descriptor_reservation() const;
  void set_direct_restore_descriptor_reservation(size_t descriptors);
  bool retain_terminal();
  bool expired(
      std::chrono::steady_clock::time_point now,
      std::chrono::steady_clock::duration pre_mutation_lifetime) const;

 private:
  std::mutex mutex_;
  State state_ = State::NEW;
  Descriptor descriptor_;
  std::unique_ptr<RestoreAdmission> restore_admission_;
  std::optional<CudaStorageBackend> restore_backend_;
  std::optional<size_t> restore_target_count_;
  std::unique_ptr<CheckpointAdmission> checkpoint_admission_;
  std::optional<CudaStorageBackend> checkpoint_backend_;
  std::optional<size_t> checkpoint_target_count_;
  uintmax_t staging_reservation_bytes_ = 0;
  size_t direct_restore_descriptor_reservation_ = 0;
  std::chrono::steady_clock::time_point staging_started_at_ = std::chrono::steady_clock::now();
  bool terminal_retained_ = false;
};
}  // namespace snapshot::pagebroker
