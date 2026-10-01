// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_transfer_engine.hpp"

#include <cstdlib>
#include <exception>
#include <filesystem>
#include <stdexcept>
#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
[[noreturn]] void
RethrowS3TransferError(std::exception_ptr exception, TransferControl control)
{
  try {
    std::rethrow_exception(exception);
  }
  catch (const TransferError&) {
    throw;
  }
  catch (const RestoreIntegrityError&) {
    throw TransferError(Failure::ARTIFACT_CORRUPT, "checkpoint checksum mismatch");
  }
  catch (const S3ResponseError&) {
    throw TransferError(Failure::ARTIFACT_CORRUPT, "invalid checkpoint object size");
  }
  catch (const TransferInterrupted& error) {
    throw TransferError(error.reason == TransferInterrupted::Reason::DEADLINE_EXCEEDED ?
        Failure::TRANSACTION_EXPIRED : Failure::STORAGE_UNAVAILABLE, "checkpoint operation interrupted");
  }
  catch (const S3Error& error) {
    const bool expired = TransferControl::Clock::now() >= control.deadline || error.primary.code == "DeadlineExceeded";
    throw TransferError(expired ? Failure::TRANSACTION_EXPIRED :
        error.primary.http_status == 401 || error.primary.http_status == 403 ? Failure::ACCESS_DENIED : Failure::STORAGE_UNAVAILABLE,
        "S3 checkpoint operation failed");
  }
  catch (const std::invalid_argument&) {
    throw TransferError(Failure::INVALID_REQUEST, "invalid checkpoint request or source");
  }
  catch (const std::exception&) {
    throw TransferError(Failure::STORAGE_ERROR, "checkpoint storage operation failed");
  }
}

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

ModelStreamerTransferEngine::ModelStreamerTransferEngine(S3Config config)
    : ModelStreamerTransferEngine("/", config.transfer)
{
  store_ = std::make_unique<S3StorageBackend>(std::move(config));
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

const S3StorageBackend&
ModelStreamerTransferEngine::ArtifactStorage() const
{
  if (!store_)
    throw std::invalid_argument("artifact storage is not configured");
  ValidateS3Configuration();
  return *store_;
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

// Reuse a healthy coordinator; replace it after a terminal native failure.
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

void
ModelStreamerTransferEngine::StageRestore(const RestorePlan& plan, const Path& destination, TransferControl control) const
{
  if (s3_options_)
    ValidateS3Configuration();
  const auto restore = AcquireRestore();
  if (!store_) {
    restore->Stage(plan, destination, control);
    return;
  }
  try {
    restore->Stage(plan, destination, control);
    control.Check();
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), control);
  }
}

PublishedArtifact
ModelStreamerTransferEngine::ResolveArtifactTarget(const ArtifactTarget& target) const
{
  return ArtifactStorage().ResolveTarget(target);
}

void
ModelStreamerTransferEngine::ValidateArtifact(const PublishedArtifact& artifact) const
{
  ArtifactStorage().ValidateCheckpoint(artifact);
}

RestorePlan
ModelStreamerTransferEngine::PrepareRestore(const StorageBackend& source, TransferControl control,
    const PublishedArtifact* artifact, bool metadata_only) const
{
  if (!artifact) {
    if (store_ || metadata_only)
      throw std::invalid_argument("configured engine requires an artifact");
    return filesystem_storage::BuildRestorePlan(filesystem_storage::SourcePath(source, storage_root_), control);
  }
  try {
    return ArtifactStorage().LoadRestorePlan(*artifact, metadata_only, control);
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), control);
  }
}

void
ModelStreamerTransferEngine::ValidateCheckpointDestination(const StorageBackend& destination,
    const PublishedArtifact* artifact, TransferControl control) const
{
  if (!artifact) {
    if (store_)
      throw std::invalid_argument("configured engine requires an artifact");
    filesystem_storage::DestinationPath(destination, storage_root_);
    return;
  }
  try {
    ArtifactStorage().PrepareCheckpoint(*artifact, control);
    control.Check();
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), control);
  }
}

RestorePlan
ModelStreamerTransferEngine::InspectCheckpoint(const Path& source, const PublishedArtifact* artifact,
    TransferControl control) const
{
  if (!artifact) {
    if (store_)
      throw std::invalid_argument("configured engine requires an artifact");
    return TransferEngine::InspectCheckpoint(source, nullptr, control);
  }
  try {
    ArtifactStorage().PrepareCheckpoint(*artifact, control);
    return ArtifactStorage().InspectCheckpoint(source, *artifact, control);
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), control);
  }
}

void
ModelStreamerTransferEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination, RestorePlan plan,
    CheckpointPublication* publication, TransferControl control) const
{
  if (!publication) {
    if (store_)
      throw std::invalid_argument("configured engine requires an artifact");
    filesystem_storage::PublishCheckpoint(source, destination, storage_root_);
    return;
  }
  try {
    const auto& storage = ArtifactStorage();
    if (publication->pending_index.empty()) {
      storage.PrepareCheckpoint(publication->artifact, control);
      publication->pending_index = storage.UploadCheckpointPayloads(source, std::move(plan), control);
    }
    // An uncertain publication retries the exact index, even if it now exists remotely.
    storage.PublishIndex(publication->artifact, publication->pending_index, control);
    publication->published = true;
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), control);
  }
}

}  // namespace snapshot::pagebroker
