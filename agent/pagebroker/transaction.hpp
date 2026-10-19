// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <chrono>
#include <condition_variable>
#include <memory>
#include <mutex>
#include <set>
#include <system_error>
#include <variant>
#include <string>
#include <vector>

#include "checkpoint_transaction_descriptor.hpp"
#include "cancellation.hpp"
#include "restore_transaction_descriptor.hpp"
#include "gpu/engine.hpp"

namespace snapshot::pagebroker {
// Shared admission accounting. Transactions hold reservations until staging is removed.
struct TransactionResources {
  TransactionResources(uintmax_t bytes, unsigned transactions)
      : byte_limit(bytes), transaction_limit(transactions) {}
  const uintmax_t byte_limit;
  const unsigned transaction_limit;
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
  enum class State { NEW, PREPARING, STAGED, ABORTING, COMMITTED, ABORTED };
  using Descriptor = std::variant<std::monostate, RestoreTransactionDescriptor, CheckpointTransactionDescriptor>;

  struct GpuRequest {
    gpu::Direction direction;
    std::vector<uint32_t> captured_pids;
    std::vector<std::string> visible_devices;
    std::vector<gpu::DeviceMapping> device_map;
    std::vector<gpu::Participant> targets;

    bool operator==(const GpuRequest&) const = default;
  };
  struct GpuOperation {
    enum class State { Ready, Running, Finished };

    gpu::ArtifactPtr artifact;
    GpuRequest request;
    // Own the pidfds referenced by request.targets, including conflict retries.
    std::vector<FileDescriptor> target_descriptors;
    CancellationPtr cancellation;
    State state = State::Ready;
    Response result;
    std::condition_variable completed;
  };
  using GpuOperationPtr = std::shared_ptr<GpuOperation>;
  // The pointer is protected by mutex(); cancellation is safe to signal independently.
  GpuOperationPtr gpu_operation;

  // Callers hold mutex() while accessing transaction state.
  std::mutex& mutex();
  State state() const;
  void set_state(State state);
  const Descriptor& descriptor() const;
  void set_descriptor(Descriptor descriptor);
  void clear_descriptor();
  bool retain_terminal();
  bool expired(std::chrono::steady_clock::time_point now) const;

  // Admission and initial byte reservation share one resource lock when limits are enabled.
  void PrepareTransfer(std::shared_ptr<TransactionResources> resources, uintmax_t initial_bytes,
      const Path& directory, const PublishedArtifact* artifact = nullptr,
      std::chrono::steady_clock::duration lifetime = std::chrono::seconds{7500});
  // Records exclusive ownership after the capture directory has been created.
  void OwnDirectory(const Path& directory);

  void Reserve(uintmax_t bytes);
  Cancellation cancellation() const { return cancellation_.WithDeadline(deadline_); }
  bool has_limits() const { return resources_ != nullptr; }
  CheckpointPublication* publication() { return publication_ ? &*publication_ : nullptr; }
  void CancelWork(); // Thread-safe; call before waiting for mutex().
  // Only the recorded directory may be removed. A matching empty path still releases reservations.
  void RemoveStaging(const Path& directory);
  void RemoveStaging(const Path& directory, std::error_code& error);
  void CleanupTransfer();
  void CleanupTransfer(std::error_code& error);
  // Marks expiry before cleanup. The broker must drain GPU work before removing staging.
  bool ExpireTransfer(std::chrono::steady_clock::time_point now);

 private:
  // Cleanup ownership exists independently of optional admission limits.
  bool HasPendingCleanup() const;
  // Allows one terminal retention only after staging and reservations are released.
  bool CanRetainTerminal() const;
  void ReleaseReservation() noexcept;
  void ReleaseAbortedDestination();
  std::mutex mutex_;
  State state_ = State::NEW;
  Descriptor descriptor_;
  bool terminal_retained_ = false;
  Cancellation cancellation_;
  Cancellation::Clock::time_point deadline_ = Cancellation::Clock::time_point::max();
  std::shared_ptr<TransactionResources> resources_;
  uintmax_t reserved_ = 0;
  bool released_ = false;
  std::optional<CheckpointPublication> publication_;
  Path transfer_directory_;
  bool transfer_expired_ = false;
};
}  // namespace snapshot::pagebroker
