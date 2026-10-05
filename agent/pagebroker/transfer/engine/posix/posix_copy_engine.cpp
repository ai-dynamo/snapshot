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
PosixCopyEngine::PrepareRestore(const StorageBackend& source, TransferControl control) const
{
  return filesystem_storage::BuildRestorePlan(filesystem_storage::SourcePath(source, storage_root_), control);
}

void
PosixCopyEngine::StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control) const
{
  filesystem_storage::StageRestore(plan, destination, control);
}

void
PosixCopyEngine::ValidateCheckpointDestination(const StorageBackend& destination) const
{
  filesystem_storage::DestinationPath(destination, storage_root_);
}

bool
PosixCopyEngine::CheckpointDestinationConflicts(const StorageBackend& destination) const
{
  return filesystem_storage::CheckpointDestinationConflicts(destination, storage_root_);
}

void
PosixCopyEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination) const
{
  filesystem_storage::PublishCheckpoint(source, destination, storage_root_);
}
}  // namespace snapshot::pagebroker
