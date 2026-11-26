// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/posix/posix_copy_engine.hpp"

#include <filesystem>
#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;

namespace {
constexpr char kNodeCacheArtifactsDirectory[] = "artifacts";

Path
CanonicalPath(Path path)
{
  return path.empty() ? Path{} : fs::weakly_canonical(std::move(path));
}
}  // namespace

PosixCopyEngine::PosixCopyEngine(Path storage_root, Path node_cache_root)
    : storage_root_(CanonicalPath(std::move(storage_root)))
{
  if (!node_cache_root.empty()) {
    node_cache_root_ = CanonicalPath(std::move(node_cache_root));
    node_cache_artifacts_ = node_cache_root_ / kNodeCacheArtifactsDirectory;
  }
}

const Path&
PosixCopyEngine::RestoreRoot(const StorageBackend& source) const
{
  // Only copies under artifacts/ are sources. The root is the node cache
  // itself, so the storage helpers also reject a symlinked artifacts/.
  if (filesystem_storage::IsWithinRoot(source, node_cache_artifacts_))
    return node_cache_root_;
  if (storage_root_.empty())
    throw std::invalid_argument("source must be within the node cache");
  return storage_root_;
}

IoEngine
PosixCopyEngine::type() const
{
  return IoEngine::POSIX_COPY;
}

PreparedRestore
PosixCopyEngine::PrepareRestore(const StorageBackend& source, Cancellation cancellation,
    const PublishedArtifact* artifact, bool metadata_only, TransferMode mode) const
{
  if (artifact || metadata_only)
    throw std::invalid_argument("POSIX copy requires filesystem storage");
  return filesystem_storage::PrepareRestore(source, RestoreRoot(source), mode, cancellation);
}

RestoreAccess
PosixCopyEngine::StageRestore(PreparedRestore preparation, const Path& destination, Cancellation cancellation) const
{
  if (preparation.mode == TransferMode::Staged)
    filesystem_storage::StageRestore(preparation.inventory, destination, cancellation);
  else
    cancellation.ThrowIfCancelled();
  return filesystem_storage::CompleteRestore(std::move(preparation), destination);
}

Path
PosixCopyEngine::PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
    const std::string& transaction_id, TransferMode mode, const PublishedArtifact* artifact, Cancellation cancellation) const
{
  ValidateCheckpointDestination(destination, artifact, cancellation);
  return filesystem_storage::PrepareCheckpoint(destination, storage_root_, staging_directory, transaction_id, mode);
}

void
PosixCopyEngine::ValidateCheckpointDestination(const StorageBackend& destination,
    const PublishedArtifact* artifact, Cancellation cancellation) const
{
  cancellation.ThrowIfCancelled();
  if (artifact || storage_root_.empty())
    throw std::invalid_argument("POSIX copy requires filesystem storage");
  filesystem_storage::DestinationPath(destination, storage_root_);
}

CheckpointInventory
PosixCopyEngine::InspectCheckpoint(const Path& source, const PublishedArtifact* artifact, Cancellation cancellation) const
{
  if (artifact)
    throw std::invalid_argument("POSIX copy requires filesystem storage");
  return filesystem_storage::BuildCheckpointInventory(source, cancellation);
}

void
PosixCopyEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination, CheckpointInventory inventory,
    CheckpointPublication* publication, Cancellation cancellation, TransferMode mode) const
{
  ValidateCheckpointDestination(destination, publication ? &publication->artifact : nullptr, cancellation);
  filesystem_storage::PublishCheckpoint(source, destination, storage_root_, inventory, mode, cancellation);
}
}  // namespace snapshot::pagebroker
