// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transaction.hpp"

#include <utility>

namespace snapshot::pagebroker {
std::mutex&
Transaction::mutex()
{
  return mutex_;
}

Transaction::State
Transaction::state() const
{
  return state_;
}

bool
Transaction::ShouldRecordExpiry(State next_state) const
{
  if (!resources_ || next_state == State::COMMITTED)
    return false;
  return expired(std::chrono::steady_clock::now(), {});
}

void
Transaction::set_state(State state)
{
  if (ShouldRecordExpiry(state))
    transfer_expired_ = true;
  state_ = state;
  if (state == State::PREPARING)
    staging_started_at_ = std::chrono::steady_clock::now();
}

const Transaction::Descriptor&
Transaction::descriptor() const
{
  return descriptor_;
}

void
Transaction::set_descriptor(Descriptor descriptor)
{
  descriptor_ = std::move(descriptor);
}

void
Transaction::clear_descriptor()
{
  // A failed remote-staging cleanup must remain retryable and retain its lease.
  if (resources_ && !released_)
    return;
  descriptor_ = std::monostate();
}

bool
Transaction::CanRetainTerminal() const
{
  if (terminal_retained_)
    return false;
  if (state_ != State::COMMITTED && state_ != State::ABORTED)
    return false;
  return !resources_ || released_;
}

bool
Transaction::retain_terminal()
{
  if (!CanRetainTerminal())
    return false;
  terminal_retained_ = true;
  return true;
}

bool
Transaction::expired(std::chrono::steady_clock::time_point now, std::chrono::steady_clock::duration lifetime) const
{
  if (resources_)
    return state_ != State::COMMITTED && (transfer_expired_ || (state_ != State::ABORTED && now >= deadline_));
  return state_ == State::STAGED && now - staging_started_at_ >= lifetime;
}

Transaction::~Transaction()
{
  ReleaseReservation();
  if (resources_ && publication_) {
    std::lock_guard lock(resources_->mutex);
    resources_->destinations.erase(publication_->artifact.artifact_handle());
  }
}

void
Transaction::PrepareTransfer(std::shared_ptr<TransactionResources> resources, Kind kind,
    const Path& directory, const PublishedArtifact* artifact)
{
  kind_ = kind;
  transfer_directory_ = directory;
  if (!resources)
    return;
  if (kind == Kind::CHECKPOINT && artifact)
    publication_ = CheckpointPublication{*artifact, {}, false};
  std::lock_guard lock(resources->mutex);
  if (resources->active >= resources->transaction_limit || resources->reserved >= resources->byte_limit)
    throw TransferError(Failure::INSUFFICIENT_STORAGE, "staging admission limit reached");
  if (publication_ && !resources->destinations.insert(publication_->artifact.artifact_handle()).second)
    throw TransferError(Failure::TRANSACTION_CONFLICT, "checkpoint ID is already owned by a transaction");
  resources_ = std::move(resources);
  deadline_ = TransferControl::Clock::now() + resources_->lifetime;
  ++resources_->active;
  reserved_ = kind == Kind::CHECKPOINT ? 1 : 0;
  resources_->reserved += reserved_;
}

void
Transaction::Reserve(uintmax_t bytes)
{
  if (!resources_)
    return;
  std::lock_guard lock(resources_->mutex);
  if (bytes <= reserved_)
    return;
  const auto additional = bytes - reserved_;
  if (additional > resources_->byte_limit - resources_->reserved)
    throw TransferError(Failure::INSUFFICIENT_STORAGE, "checkpoint exceeds staging budget");
  resources_->reserved += additional;
  reserved_ = bytes;
}

void
Transaction::ReleaseReservation() noexcept
{
  if (!resources_ || released_)
    return;
  if (publication_)
    std::string().swap(publication_->pending_index);
  std::lock_guard lock(resources_->mutex);
  resources_->reserved -= reserved_;
  --resources_->active;
  reserved_ = 0;
  released_ = true;
}

void
Transaction::CancelTransfer()
{
  cancellation_.request_stop();
}

void
Transaction::RemoveStaging(const Path& directory)
{
  namespace fs = std::filesystem;
  if (directory == transfer_directory_) {
    // Restore applies saved modes after verifying data. Make only our owned
    // tree traversable/writable again before releasing it.
    const auto options = fs::perm_options::replace | fs::perm_options::nofollow;
    if (fs::is_directory(fs::symlink_status(directory))) {
      fs::permissions(directory, fs::perms::owner_all, options);
      for (const auto& entry : fs::recursive_directory_iterator(directory)) {
        if (fs::is_directory(entry.symlink_status()))
          fs::permissions(entry.path(), fs::perms::owner_all, options);
      }
    }
  }
  fs::remove_all(directory);
  if (directory == transfer_directory_) {
    ReleaseReservation();
    if (state_ == State::COMMITTED || state_ == State::ABORTED)
      clear_descriptor();
  }
}

void
Transaction::RemoveStaging(const Path& directory, std::error_code& error)
{
  try {
    RemoveStaging(directory);
    error.clear();
  }
  catch (const std::filesystem::filesystem_error& failure) {
    error = failure.code();
  }
}

void
Transaction::CleanupTransfer()
{
  if (resources_ && !released_)
    RemoveStaging(transfer_directory_);
}

void
Transaction::CleanupTransfer(std::error_code& error)
{
  error.clear();
  if (resources_ && !released_)
    RemoveStaging(transfer_directory_, error);
}

bool
Transaction::ReleaseExpiredTransfer()
{
  if (!resources_ || !expired(std::chrono::steady_clock::now(), {}))
    return false;
  CancelTransfer();
  CleanupTransfer();
  clear_descriptor();
  set_state(State::ABORTED);
  return true;
}

}  // namespace snapshot::pagebroker
