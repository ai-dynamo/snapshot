// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "posix_copy_engine.hpp"

#include <filesystem>
#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;

PosixCopyEngine::PosixCopyEngine(Path storage_root)
    : storage_root_(fs::weakly_canonical(std::move(storage_root)))
{
}

TransferEngineType
PosixCopyEngine::type() const
{
  return TransferEngineType::POSIX_COPY;
}

RestorePlan
PosixCopyEngine::PrepareRestore(const StorageBackend& source, TransferControl control,
    const PublishedArtifact* artifact, bool metadata_only) const
{
  if (artifact || metadata_only)
    throw std::invalid_argument("POSIX copy requires filesystem storage");
  return filesystem_storage::BuildRestorePlan(filesystem_storage::SourcePath(source, storage_root_), control);
}

void
PosixCopyEngine::StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control) const
{
  filesystem_storage::StageRestore(plan, destination, control);
}

void
PosixCopyEngine::ValidateCheckpointDestination(const StorageBackend& destination,
    const PublishedArtifact* artifact, TransferControl control) const
{
  control.Check();
  if (artifact)
    throw std::invalid_argument("POSIX copy requires filesystem storage");
  filesystem_storage::DestinationPath(destination, storage_root_);
}

void
PosixCopyEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan,
    CheckpointPublication* publication, TransferControl control) const
{
  ValidateCheckpointDestination(destination, publication ? &publication->artifact : nullptr, control);
  filesystem_storage::PublishCheckpoint(source, destination, storage_root_);
}
}  // namespace snapshot::pagebroker
