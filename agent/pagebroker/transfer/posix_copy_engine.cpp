// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "posix_copy_engine.hpp"

#include <filesystem>
#include <utility>

#include "filesystem_storage.hpp"

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

uintmax_t
PosixCopyEngine::RestoreSize(const StorageBackend& source) const
{
  return filesystem_storage::RestoreSize(source, storage_root_);
}

void
PosixCopyEngine::StageRestore(const StorageBackend& source, const Path& destination) const
{
  fs::copy(filesystem_storage::SourcePath(source, storage_root_), destination, fs::copy_options::recursive);
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
