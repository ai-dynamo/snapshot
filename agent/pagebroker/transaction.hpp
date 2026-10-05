// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <memory>
#include <mutex>
#include <stop_token>
#include <set>
#include <system_error>
#include <variant>

#include "checkpoint_transaction_descriptor.hpp"
#include "restore_transaction_descriptor.hpp"

namespace snapshot::pagebroker {
// Shared admission accounting. Transactions hold reservations until staging is removed.
struct TransactionResources {
  TransactionResources(uintmax_t bytes, unsigned transactions, std::chrono::steady_clock::duration lifetime)
      : byte_limit(bytes), transaction_limit(transactions), lifetime(lifetime) {}
  const uintmax_t byte_limit;
  const unsigned transaction_limit;
  const std::chrono::steady_clock::duration lifetime;
  std::mutex mutex;
  uintmax_t reserved = 0;
  unsigned active = 0;
  std::set<std::string> destinations;
};

class Transaction {
 public:
  Transaction() = default;
  ~Transaction();
  Transaction(const Transaction&) = delete;
  Transaction& operator=(const Transaction&) = delete;
  enum class Kind { CHECKPOINT, RESTORE, METADATA };
  enum class State { NEW, PREPARING, STAGED, COMMITTED, ABORTED };
  using Descriptor = std::variant<std::monostate, RestoreTransactionDescriptor, CheckpointTransactionDescriptor>;

  // Callers hold mutex() while accessing transaction state.
  std::mutex& mutex();
  State state() const;
  void set_state(State state);
  const Descriptor& descriptor() const;
  void set_descriptor(Descriptor descriptor);
  void clear_descriptor();
  bool retain_terminal();
  bool expired(std::chrono::steady_clock::time_point now, std::chrono::steady_clock::duration lifetime) const;

  void PrepareTransfer(std::shared_ptr<TransactionResources> resources, Kind kind,
      const Path& directory, const PublishedArtifact* artifact = nullptr);
  void Reserve(uintmax_t bytes);
  TransferControl control() const { return {deadline_, cancellation_.get_token()}; }
  bool has_limits() const { return resources_ != nullptr; }
  Kind kind() const { return kind_; }
  CheckpointPublication* publication() { return publication_ ? &*publication_ : nullptr; }
  void CancelTransfer(); // Thread-safe; call before waiting for mutex().
  void RemoveStaging(const Path& directory);
  void RemoveStaging(const Path& directory, std::error_code& error);
  void CleanupTransfer();
  void CleanupTransfer(std::error_code& error);
  bool ReleaseExpiredTransfer();

 private:
  // Decides whether a state change must remember an expired transfer deadline.
  bool ShouldRecordExpiry(State next_state) const;
  // Allows one terminal retention only after any transfer reservation is released.
  bool CanRetainTerminal() const;
  void ReleaseReservation() noexcept;
  std::mutex mutex_;
  State state_ = State::NEW;
  Descriptor descriptor_;
  std::chrono::steady_clock::time_point staging_started_at_;
  bool terminal_retained_ = false;
  std::stop_source cancellation_;
  Kind kind_ = Kind::RESTORE;
  TransferControl::Clock::time_point deadline_ = TransferControl::Clock::time_point::max();
  std::shared_ptr<TransactionResources> resources_;
  uintmax_t reserved_ = 0;
  bool released_ = false;
  std::optional<CheckpointPublication> publication_;
  Path transfer_directory_;
  bool transfer_expired_ = false;
};
}  // namespace snapshot::pagebroker
