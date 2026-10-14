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
    : storage_root_(fs::weakly_canonical(std::move(storage_root))), restore_(CreateRestore())
{
}

std::shared_ptr<ModelStreamerRestore>
ModelStreamerTransferEngine::CreateRestore() const
{
  return std::make_shared<ModelStreamerRestore>();
}

std::shared_ptr<ModelStreamerRestore>
ModelStreamerTransferEngine::AcquireRestore() const
{
  for (;;) {
    std::shared_ptr<ModelStreamerRestore> current;
    {
      std::lock_guard lock(restore_mutex_);
      current = restore_;
    }
    if (!current->Failed())
      return current;

    auto replacement = CreateRestore();
    {
      std::lock_guard lock(restore_mutex_);
      if (restore_ == current) {
        restore_ = replacement;
        return replacement;
      }
    }
  }
}

IoEngine
ModelStreamerTransferEngine::type() const
{
  return IoEngine::MODEL_STREAMER;
}

PreparedRestore
ModelStreamerTransferEngine::PrepareRestore(const StorageBackend& source, TransferControl control,
    const PublishedArtifact* artifact, bool metadata_only, TransferMode mode) const
{
  if (artifact || metadata_only)
    throw std::invalid_argument("Model Streamer requires filesystem storage");
  return filesystem_storage::PrepareRestore(source, storage_root_, mode, control);
}

RestoreAccess
ModelStreamerTransferEngine::StageRestore(PreparedRestore preparation, const Path& destination, TransferControl control) const
{
  control.Check();
  if (preparation.mode == TransferMode::Staged)
    AcquireRestore()->Stage(preparation.plan, destination, control);
  return filesystem_storage::CompleteRestore(std::move(preparation), destination);
}

Path
ModelStreamerTransferEngine::PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
    const std::string& transaction_id, TransferMode mode, const PublishedArtifact* artifact, TransferControl control) const
{
  ValidateCheckpointDestination(destination, artifact, control);
  return filesystem_storage::PrepareCheckpoint(destination, storage_root_, staging_directory, transaction_id, mode);
}

void
ModelStreamerTransferEngine::ValidateCheckpointDestination(const StorageBackend& destination,
    const PublishedArtifact* artifact, TransferControl control) const
{
  control.Check();
  if (artifact)
    throw std::invalid_argument("Model Streamer requires filesystem storage");
  filesystem_storage::DestinationPath(destination, storage_root_);
}

RestorePlan
ModelStreamerTransferEngine::InspectCheckpoint(const Path& source, const PublishedArtifact* artifact, TransferControl control) const
{
  if (artifact)
    throw std::invalid_argument("Model Streamer requires filesystem storage");
  return filesystem_storage::BuildRestorePlan(source, control);
}

void
ModelStreamerTransferEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan plan,
    CheckpointPublication* publication, TransferControl control, TransferMode mode) const
{
  ValidateCheckpointDestination(destination, publication ? &publication->artifact : nullptr, control);
  filesystem_storage::PublishCheckpoint(source, destination, storage_root_, plan, mode, control);
}
}  // namespace snapshot::pagebroker
