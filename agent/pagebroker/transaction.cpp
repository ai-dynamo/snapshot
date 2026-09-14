// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transaction.hpp"

#include <stdexcept>
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

Transaction::Descriptor&
Transaction::descriptor()
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

void
Transaction::set_restore_admission(
    std::unique_ptr<RestoreAdmission> admission, CudaStorageBackend backend,
    size_t target_count)
{
  if (admission == nullptr || target_count == 0)
    throw std::invalid_argument("restore admission and target count are required");
  restore_admission_ = std::move(admission);
  restore_backend_ = backend;
  restore_target_count_ = target_count;
}

std::unique_ptr<RestoreAdmission>
Transaction::take_restore_admission()
{
  return std::move(restore_admission_);
}

bool
Transaction::has_restore_admission() const
{
  return restore_admission_ != nullptr;
}

std::optional<CudaStorageBackend>
Transaction::restore_backend() const
{
  return restore_backend_;
}

std::optional<size_t>
Transaction::restore_target_count() const
{
  return restore_target_count_;
}

void
Transaction::clear_restore_admission()
{
  restore_admission_.reset();
  restore_backend_.reset();
  restore_target_count_.reset();
}

void
Transaction::set_checkpoint_admission(
    std::unique_ptr<CheckpointAdmission> admission,
    CudaStorageBackend backend,
    size_t target_count)
{
  if (admission == nullptr || target_count == 0)
    throw std::invalid_argument("checkpoint admission and target count are required");
  checkpoint_admission_ = std::move(admission);
  checkpoint_backend_ = backend;
  checkpoint_target_count_ = target_count;
}

std::unique_ptr<CheckpointAdmission>
Transaction::take_checkpoint_admission()
{
  return std::move(checkpoint_admission_);
}

bool
Transaction::has_checkpoint_admission() const
{
  return checkpoint_admission_ != nullptr;
}

std::optional<CudaStorageBackend>
Transaction::checkpoint_backend() const
{
  return checkpoint_backend_;
}

std::optional<size_t>
Transaction::checkpoint_target_count() const
{
  return checkpoint_target_count_;
}

void
Transaction::clear_checkpoint_admission()
{
  checkpoint_admission_.reset();
  checkpoint_backend_.reset();
  checkpoint_target_count_.reset();
}

uintmax_t
Transaction::staging_reservation_bytes() const
{
  return staging_reservation_bytes_;
}

void
Transaction::set_staging_reservation_bytes(uintmax_t bytes)
{
  staging_reservation_bytes_ = bytes;
}

size_t
Transaction::direct_restore_descriptor_reservation() const
{
  return direct_restore_descriptor_reservation_;
}

void
Transaction::set_direct_restore_descriptor_reservation(size_t descriptors)
{
  direct_restore_descriptor_reservation_ = descriptors;
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
Transaction::expired(
    std::chrono::steady_clock::time_point now,
    std::chrono::steady_clock::duration pre_mutation_lifetime) const
{
  switch (state_) {
    case State::NEW:
    case State::PREPARING:
    case State::STAGED:
      return now - staging_started_at_ >= pre_mutation_lifetime;
    case State::CHECKPOINT_ADMITTED:
    case State::RESTORE_ADMITTED:
    case State::CUDA_STARTED:
    case State::CUDA_COMPLETE:
    case State::PUBLISHED_CLEANUP_PENDING:
      // These states may follow workload mutation or externally visible
      // publication. Only an explicit Commit/Abort or coordinated shutdown may
      // release them; elapsed wall time is not durable cleanup proof.
    case State::COMMITTED:
    case State::ABORTED:
      return false;
  }
  return false;
}

}  // namespace snapshot::pagebroker
