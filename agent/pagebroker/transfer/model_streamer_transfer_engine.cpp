// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_transfer_engine.hpp"

#include <filesystem>
#include <stdexcept>
#include <string>
#include <utility>

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
Path
StoragePath(const StorageBackend& storage, const Path& storage_root, const char* label)
{
  if (!storage.has_filesystem() || storage.filesystem().directory().empty())
    throw std::invalid_argument(std::string("filesystem ") + label + " is required");
  const Path path(storage.filesystem().directory());
  const Path relative = path.lexically_relative(storage_root);
  if (!path.is_absolute() || path.lexically_normal() != path || relative.empty() ||
      relative == "." || relative.string().starts_with("../") || relative == "..")
    throw std::invalid_argument(std::string(label) + " must be within storage root");

  Path component = storage_root;
  for (const auto& part : relative) {
    component /= part;
    if (fs::is_symlink(component))
      throw std::invalid_argument(std::string(label) + " contains symlink");
  }
  return path;
}

Path
SourcePath(const StorageBackend& source, const Path& storage_root)
{
  const Path path = StoragePath(source, storage_root, "source");
  if (!fs::is_directory(path))
    throw std::invalid_argument("source must be a storage directory");
  return path;
}

RestorePlan
BuildFilesystemRestorePlan(const Path& source)
{
  RestorePlan plan;
  plan.root_permissions = fs::symlink_status(source).permissions();
  for (const auto& entry : fs::recursive_directory_iterator(source)) {
    const auto status = entry.symlink_status();
    if (fs::is_symlink(status))
      throw std::runtime_error("checkpoint contains symlink");

    const Path relative = entry.path().lexically_relative(source);
    if (relative.empty() || relative == "." || relative == ".." || relative.string().starts_with("../"))
      throw std::runtime_error("checkpoint contains invalid path");
    if (fs::is_directory(status)) {
      plan.directories.push_back(RestoreDirectory{relative, status.permissions()});
      continue;
    }
    if (!fs::is_regular_file(status))
      throw std::runtime_error("checkpoint contains unsupported file type");

    plan.files.push_back(
        RestoreFile{entry.path().string(), relative, entry.file_size(), status.permissions()});
  }
  return plan;
}
}  // namespace

ModelStreamerTransferEngine::ModelStreamerTransferEngine(Path storage_root)
    : storage_root_(fs::weakly_canonical(std::move(storage_root))), posix_(storage_root_),
      restore_(std::make_shared<ModelStreamerRestore>())
{
}

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

uintmax_t
ModelStreamerTransferEngine::RestoreSize(const StorageBackend& source) const
{
  return posix_.RestoreSize(source);
}

void
ModelStreamerTransferEngine::StageRestore(const StorageBackend& source, const Path& destination) const
{
  const auto restore = AcquireRestore();
  restore->Stage(BuildFilesystemRestorePlan(SourcePath(source, storage_root_)), destination);
}

void
ModelStreamerTransferEngine::ValidateCheckpointDestination(const StorageBackend& destination) const
{
  posix_.ValidateCheckpointDestination(destination);
}

bool
ModelStreamerTransferEngine::CheckpointDestinationConflicts(const StorageBackend& destination) const
{
  return posix_.CheckpointDestinationConflicts(destination);
}

void
ModelStreamerTransferEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination) const
{
  posix_.PublishCheckpoint(source, destination);
}
}  // namespace snapshot::pagebroker
