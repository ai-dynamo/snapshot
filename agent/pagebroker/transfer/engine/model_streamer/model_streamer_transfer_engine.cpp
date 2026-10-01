// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_transfer_engine.hpp"

#include <filesystem>
#include <stdexcept>
#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;

ModelStreamerTransferEngine::ModelStreamerTransferEngine(Path storage_root)
    : storage_root_(fs::weakly_canonical(std::move(storage_root))),
      restore_(std::make_shared<ModelStreamerRestore>())
{
}

// Reuse a healthy session; replace a failed one after its native streamer stops.
// Existing callers keep the old wrapper until they finish cleanup.
std::shared_ptr<ModelStreamerRestore>
ModelStreamerTransferEngine::AcquireRestore() const
{
  std::shared_ptr<ModelStreamerRestore> previous;
  std::shared_ptr<ModelStreamerRestore> current;
  {
    std::lock_guard lock(restore_mutex_);
    if (restore_->Failed()) {
      auto replacement = std::make_shared<ModelStreamerRestore>();
      previous = std::move(restore_);
      restore_ = std::move(replacement);
    }
    current = restore_;
  }
  // Destroying a failed generation joins its event-loop thread. Keep that
  // potentially blocking work outside the engine's short-lived pointer lock.
  previous.reset();
  return current;
}

TransferEngineType
ModelStreamerTransferEngine::type() const
{
  return TransferEngineType::MODEL_STREAMER;
}

RestorePlan
ModelStreamerTransferEngine::PrepareRestore(const StorageBackend& source, TransferControl control) const
{
  return filesystem_storage::BuildRestorePlan(filesystem_storage::SourcePath(source, storage_root_), control);
}

void
ModelStreamerTransferEngine::StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control) const
{
  const auto restore = AcquireRestore();
  restore->Stage(plan, destination, control);
}

void
ModelStreamerTransferEngine::ValidateCheckpointDestination(const StorageBackend& destination) const
{
  filesystem_storage::DestinationPath(destination, storage_root_);
}

bool
ModelStreamerTransferEngine::CheckpointDestinationConflicts(const StorageBackend& destination) const
{
  return filesystem_storage::CheckpointDestinationConflicts(destination, storage_root_);
}

void
ModelStreamerTransferEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination) const
{
  filesystem_storage::PublishCheckpoint(source, destination, storage_root_);
}
}  // namespace snapshot::pagebroker
