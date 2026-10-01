// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_transfer_engine.hpp"

#include <cstdlib>
#include <filesystem>
#include <stdexcept>
#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
bool
HasValidAddressingSetting(const std::string& addressing)
{
  if (!std::getenv("RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING"))
    return true;
  return addressing == "0" || addressing == "1";
}

std::string
Environment(const char* name)
{
  const auto* value = std::getenv(name);
  return value ? value : "";
}

}  // namespace

ModelStreamerTransferEngine::ModelStreamerTransferEngine(Path storage_root)
    : storage_root_(fs::weakly_canonical(std::move(storage_root))),
      restore_(CreateRestore())
{
}

ModelStreamerTransferEngine::ModelStreamerTransferEngine(Path storage_root, S3TransferOptions options)
    : storage_root_(fs::weakly_canonical(std::move(storage_root))), s3_options_(std::move(options)),
      restore_(CreateRestore())
{
  ValidateS3Configuration();
}

void
ModelStreamerTransferEngine::ValidateS3Configuration() const
{
  if (!s3_options_)
    throw std::invalid_argument("Model Streamer engine has no S3 configuration");
  const auto& options = *s3_options_;
  options.connection.Validate();
  options.upload_limits.Validate();
  if (options.restore_timeout <= std::chrono::milliseconds::zero() || options.restore_timeout > std::chrono::hours(24))
    throw std::invalid_argument("S3 restore timeout must be positive and at most 24 hours");

  const auto addressing = Environment("RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING");
  if (!HasValidAddressingSetting(addressing))
    throw std::invalid_argument("RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING must be 0 or 1");
  if (options.connection.use_virtual_addressing != (addressing != "0"))
    throw std::invalid_argument("S3 addressing must match RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING");
  if (options.connection.ca_file != Environment("AWS_CA_BUNDLE"))
    throw std::invalid_argument("S3 CA file must match AWS_CA_BUNDLE for both upload and restore");
}

std::shared_ptr<ModelStreamerRestore>
ModelStreamerTransferEngine::CreateRestore() const
{
  if (!s3_options_)
    return std::make_shared<ModelStreamerRestore>();
  const auto& connection = s3_options_->connection;
  return std::make_shared<ModelStreamerRestore>(
      ModelStreamerSessionOptions{connection.region, connection.endpoint, connection.access_key_id,
                                  connection.secret_access_key, connection.session_token},
      s3_options_->restore_timeout);
}

// Reuse a healthy session; replace a failed one after its native streamer stops.
// Existing callers keep the old wrapper until they finish cleanup.
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
  if (s3_options_)
    ValidateS3Configuration();
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
