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

void
Transaction::set_state(State state)
{
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
  descriptor_ = std::monostate();
}

bool
Transaction::retain_terminal()
{
  if (terminal_retained_ || (state_ != State::COMMITTED && state_ != State::ABORTED))
    return false;
  terminal_retained_ = true;
  return true;
}

bool
Transaction::expired(std::chrono::steady_clock::time_point now, std::chrono::steady_clock::duration lifetime) const
{
  return state_ == State::STAGED && now - staging_started_at_ >= lifetime;
}

void
Transaction::PrepareTransfer(const Path& directory)
{
  transfer_directory_ = directory;
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

}  // namespace snapshot::pagebroker
