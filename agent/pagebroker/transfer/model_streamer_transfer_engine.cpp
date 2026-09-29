// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_transfer_engine.hpp"

#include <filesystem>
#include <stdexcept>
#include <utility>

#include "filesystem_storage.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
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

uintmax_t
ModelStreamerTransferEngine::RestoreSize(const StorageBackend& source) const
{
  return filesystem_storage::RestoreSize(source, storage_root_);
}

void
ModelStreamerTransferEngine::StageRestore(const StorageBackend& source, const Path& destination) const
{
  const auto restore = AcquireRestore();
  restore->Stage(BuildFilesystemRestorePlan(filesystem_storage::SourcePath(source, storage_root_)), destination);
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
