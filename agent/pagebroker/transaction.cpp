// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transaction.hpp"

#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"

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

void
Transaction::set_state(State state)
{
  state_ = state;
  if ((state == State::COMMITTED || state == State::ABORTED) && gpu_operation) {
    gpu_operation->target_descriptors.clear();
  }
  ReleaseAbortedDestination();
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
  // Failed cleanup must remain retryable, including without admission limits.
  if (HasPendingCleanup())
    return;
  descriptor_ = std::monostate();
}

bool
Transaction::HasPendingCleanup() const
{
  return !transfer_directory_.empty() || (resources_ && !released_);
}

bool
Transaction::CanRetainTerminal() const
{
  if (terminal_retained_)
    return false;
  if (state_ != State::COMMITTED && state_ != State::ABORTED)
    return false;
  return !HasPendingCleanup();
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
Transaction::expired(std::chrono::steady_clock::time_point now) const
{
  return deadline_ != Cancellation::Clock::time_point::max() &&
         state_ != State::COMMITTED && state_ != State::ABORTED &&
         (transfer_expired_ || now >= deadline_);
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
Transaction::PrepareTransfer(std::shared_ptr<TransactionResources> resources, uintmax_t initial_bytes,
    const Path& directory, const PublishedArtifact* artifact, std::chrono::steady_clock::duration lifetime)
{
  transfer_directory_ = directory;
  deadline_ = Cancellation::Clock::now() + lifetime;
  if (!resources)
    return;
  if (artifact)
    publication_ = CheckpointPublication{*artifact, {}, false};
  std::lock_guard lock(resources->mutex);
  if (resources->active >= resources->transaction_limit || resources->reserved >= resources->byte_limit ||
      initial_bytes > resources->byte_limit - resources->reserved)
    throw TransferError(Failure::INSUFFICIENT_STORAGE, "staging admission limit reached");
  if (publication_ && !resources->destinations.insert(publication_->artifact.artifact_handle()).second)
    throw TransferError(Failure::TRANSACTION_CONFLICT, "checkpoint ID is already owned by a transaction");
  resources_ = std::move(resources);
  ++resources_->active;
  reserved_ = initial_bytes;
  resources_->reserved += reserved_;
}

void
Transaction::OwnDirectory(const Path& directory)
{
  transfer_directory_ = directory;
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
Transaction::ReleaseAbortedDestination()
{
  if (state_ != State::ABORTED || HasPendingCleanup() || !resources_ || !publication_)
    return;
  std::lock_guard lock(resources_->mutex);
  resources_->destinations.erase(publication_->artifact.artifact_handle());
  // Keep the aborted result, but prevent its destructor from erasing a later owner's reservation.
  publication_.reset();
}

void
Transaction::CancelWork()
{
  cancellation_.Cancel();
}

void
Transaction::RemoveStaging(const Path& directory)
{
  if (directory != transfer_directory_)
    return;

  if (!directory.empty())
    filesystem_storage::RemoveStaging(directory);

  ReleaseReservation();
  transfer_directory_.clear();
  ReleaseAbortedDestination();
  if (state_ == State::COMMITTED || state_ == State::ABORTED)
    clear_descriptor();
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
  if (HasPendingCleanup())
    RemoveStaging(transfer_directory_);
}

void
Transaction::CleanupTransfer(std::error_code& error)
{
  error.clear();
  if (HasPendingCleanup())
    RemoveStaging(transfer_directory_, error);
}

bool
Transaction::ExpireTransfer(std::chrono::steady_clock::time_point now)
{
  if (!expired(now))
    return false;
  transfer_expired_ = true;
  CancelWork();
  // Block new work while cleanup retries or GPU work drains.
  set_state(State::ABORTING);
  return true;
}

}  // namespace snapshot::pagebroker
