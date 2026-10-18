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
  catch (const S3ResponseError&) {
    throw TransferError(Failure::ARTIFACT_CORRUPT, "invalid checkpoint object size");
  }
  catch (const TransferInterrupted& error) {
    throw TransferError(TransferFailureCode(error), error.what());
  }
  catch (const S3Error& error) {
    const bool expired = TransferControl::Clock::now() >= control.deadline || error.primary.code == "DeadlineExceeded";
    throw TransferError(expired ? Failure::TRANSACTION_EXPIRED :
        error.primary.code == "Cancelled" ? Failure::INTERNAL_ERROR :
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

ModelStreamerTransferEngine::ModelStreamerTransferEngine(Path storage_root, ModelStreamerOptions model_streamer)
    : storage_root_(fs::weakly_canonical(std::move(storage_root))),
      model_streamer_(std::move(model_streamer)),
      restore_(CreateRestore())
{
}

ModelStreamerTransferEngine::ModelStreamerTransferEngine(Path storage_root, S3TransferOptions options, ModelStreamerOptions model_streamer)
    : storage_root_(fs::weakly_canonical(std::move(storage_root))), s3_options_(std::move(options)),
      model_streamer_(std::move(model_streamer)),
      restore_(CreateRestore())
{
  ValidateS3Configuration();
}

ModelStreamerTransferEngine::ModelStreamerTransferEngine(S3Config config, ModelStreamerOptions model_streamer)
    : ModelStreamerTransferEngine("/", config.transfer, std::move(model_streamer))
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
  model_streamer_.ValidateEnvironment();
  ModelStreamerSessionOptions session;
  session.filesystem_strategy = model_streamer_.filesystem_strategy;
  if (!s3_options_)
    return std::make_shared<ModelStreamerRestore>(std::move(session));
  const auto& connection = s3_options_->connection;
  session.region = connection.region;
  session.endpoint = connection.endpoint;
  session.access_key_id = connection.access_key_id;
  session.secret_access_key = connection.secret_access_key;
  session.session_token = connection.session_token;
  return std::make_shared<ModelStreamerRestore>(std::move(session), s3_options_->restore_timeout);
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

IoEngine
ModelStreamerTransferEngine::type() const
{
  return IoEngine::MODEL_STREAMER;
}

RestoreAccess
ModelStreamerTransferEngine::StageRestore(PreparedRestore preparation, const Path& destination, TransferControl control) const
{
  if (s3_options_)
    ValidateS3Configuration();
  const auto stage = [&] {
    control.Check();
    if (store_ && preparation.mode == TransferMode::Direct)
      throw std::invalid_argument("S3 restore requires staging");
    if (preparation.mode == TransferMode::Staged)
      AcquireRestore()->Stage(preparation.plan, destination, control);
    control.Check();
    return filesystem_storage::CompleteRestore(std::move(preparation), destination);
  };
  if (!store_)
    return stage();
  try {
    return stage();
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

PreparedRestore
ModelStreamerTransferEngine::PrepareRestore(const StorageBackend& source, TransferControl control,
    const PublishedArtifact* artifact, bool metadata_only, TransferMode mode) const
{
  if (!artifact) {
    if (store_ || metadata_only)
      throw std::invalid_argument("configured engine requires an artifact");
    return filesystem_storage::PrepareRestore(source, storage_root_, mode, control);
  }
  try {
    return ArtifactStorage().PrepareRestore(*artifact, metadata_only, mode, control);
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), control);
  }
}

Path
ModelStreamerTransferEngine::PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
    const std::string& transaction_id, TransferMode mode, const PublishedArtifact* artifact, TransferControl control) const
{
  if (!artifact) {
    ValidateCheckpointDestination(destination, nullptr, control);
    return filesystem_storage::PrepareCheckpoint(destination, storage_root_, staging_directory, transaction_id, mode);
  }
  try {
    return ArtifactStorage().PrepareCheckpoint(*artifact, staging_directory, mode, control);
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
    control.Check();
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
    return filesystem_storage::BuildRestorePlan(source, control);
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
    CheckpointPublication* publication, TransferControl control, TransferMode mode) const
{
  if (!publication) {
    if (store_)
      throw std::invalid_argument("configured engine requires an artifact");
    filesystem_storage::PublishCheckpoint(source, destination, storage_root_, plan, mode, control);
    return;
  }
  try {
    ArtifactStorage().PublishCheckpoint(source, std::move(plan), *publication, mode, control);
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), control);
  }
}

}  // namespace snapshot::pagebroker
