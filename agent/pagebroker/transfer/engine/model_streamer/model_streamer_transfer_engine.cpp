// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_transfer_engine.hpp"

#include <stdexcept>
#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {

ModelStreamerTransferEngine::ModelStreamerTransferEngine(
    ModelStreamerSessionOptions options, std::chrono::milliseconds restore_timeout)
    : options_(std::move(options)), restore_timeout_(restore_timeout), restore_(CreateRestore())
{
}

std::shared_ptr<ModelStreamerRestore>
ModelStreamerTransferEngine::CreateRestore() const
{
  return std::make_shared<ModelStreamerRestore>(options_, restore_timeout_);
}

std::shared_ptr<ModelStreamerRestore>
ModelStreamerTransferEngine::AcquireRestore() const
{
  // Declare before the lock so destruction joins the old worker after unlocking.
  std::shared_ptr<ModelStreamerRestore> previous;
  std::lock_guard lock(restore_mutex_);
  if (restore_->Failed()) {
    auto replacement = CreateRestore();
    previous = std::exchange(restore_, std::move(replacement));
  }
  return restore_;
}

IoEngine
ModelStreamerTransferEngine::type() const
{
  return IoEngine::MODEL_STREAMER;
}

RestoreAccess
ModelStreamerTransferEngine::StageRestore(PreparedRestore preparation, const Path& destination, Cancellation cancellation) const
{
  cancellation.ThrowIfCancelled();
  if (preparation.mode != TransferMode::Staged)
    throw std::invalid_argument("S3 restore requires staging");
  if (!preparation.local_source.empty() || preparation.source.get() >= 0)
    throw std::invalid_argument("S3 restore cannot borrow a local source");
  AcquireRestore()->Stage(preparation.inventory, destination, cancellation);
  cancellation.ThrowIfCancelled();
  return filesystem_storage::CompleteRestore(std::move(preparation), destination);
}
}  // namespace snapshot::pagebroker
