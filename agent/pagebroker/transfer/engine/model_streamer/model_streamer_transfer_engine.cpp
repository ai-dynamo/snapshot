// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_transfer_engine.hpp"

#include <exception>
#include <stdexcept>
#include <utility>

#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {
namespace {
[[noreturn]] void
RethrowS3TransferError(std::exception_ptr exception, Cancellation cancellation)
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
  catch (const CancellationError& error) {
    throw TransferError(TransferFailureCode(error), error.what());
  }
  catch (const S3Error& error) {
    if (const auto reason = cancellation.Reason())
      throw TransferError(TransferFailureCode(CancellationError(*reason)), "S3 checkpoint operation cancelled");
    throw TransferError(error.http_status == 401 || error.http_status == 403
                            ? Failure::ACCESS_DENIED : Failure::STORAGE_UNAVAILABLE,
                        "S3 checkpoint operation failed");
  }
  catch (const std::invalid_argument&) {
    throw TransferError(Failure::INVALID_REQUEST, "invalid checkpoint request or source");
  }
  catch (const std::exception&) {
    throw TransferError(Failure::STORAGE_ERROR, "checkpoint storage operation failed");
  }
}

}  // namespace

ModelStreamerTransferEngine::ModelStreamerTransferEngine(S3Config config, ModelStreamerOptions model_streamer)
    : store_(std::move(config)), model_streamer_(std::move(model_streamer)), restore_(CreateRestore())
{
  ValidateModelStreamerEnvironment(store_.config().transfer);
}

std::shared_ptr<ModelStreamerRestore>
ModelStreamerTransferEngine::CreateRestore() const
{
  model_streamer_.ValidateEnvironment();
  const auto& options = store_.config().transfer;
  const auto& connection = options.connection;
  return std::make_shared<ModelStreamerRestore>(
      ModelStreamerSessionOptions{connection.region, connection.endpoint, connection.access_key_id,
                                  connection.secret_access_key, connection.session_token},
      options.restore_timeout);
}

// Reuse a healthy coordinator; replace it after a terminal native failure.
// Existing callers keep the old wrapper until they finish cleanup.
std::shared_ptr<ModelStreamerRestore>
ModelStreamerTransferEngine::AcquireRestore() const
{
  // Declare before the lock so destruction joins the old worker after unlocking.
  std::shared_ptr<ModelStreamerRestore> previous;
  std::lock_guard lock(restore_mutex_);
  if (restore_->Failed()) {
    auto replacement = CreateRestore();
    previous = std::exchange(restore_, std::move(replacement));
  }
  return restore_;
}

IoEngine
ModelStreamerTransferEngine::type() const
{
  return IoEngine::MODEL_STREAMER;
}

RestoreAccess
ModelStreamerTransferEngine::StageRestore(PreparedRestore preparation, const Path& destination, Cancellation cancellation) const
{
  try {
    cancellation.ThrowIfCancelled();
    AcquireRestore()->Stage(preparation.inventory, destination, cancellation);
    cancellation.ThrowIfCancelled();
    return {destination, destination, filesystem_storage::OpenDirectory(destination)};
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), cancellation);
  }
}

PublishedArtifact
ModelStreamerTransferEngine::ResolveArtifactTarget(const ArtifactTarget& target) const
{
  return store_.ResolveTarget(target);
}

void
ModelStreamerTransferEngine::ValidateArtifact(const PublishedArtifact& artifact) const
{
  store_.ValidateArtifactDescriptor(artifact);
}

PreparedRestore
ModelStreamerTransferEngine::PrepareRestore(const StorageBackend& source, Cancellation cancellation,
    const PublishedArtifact* artifact, bool metadata_only, TransferMode mode) const
{
  if (!artifact || source.has_filesystem())
    throw std::invalid_argument("S3 restore requires an artifact source");
  try {
    return store_.PrepareRestore(*artifact, metadata_only, mode, cancellation);
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), cancellation);
  }
}

Path
ModelStreamerTransferEngine::PrepareCheckpoint(const StorageBackend& destination, const Path& staging_directory,
    const std::string&, TransferMode mode, const PublishedArtifact* artifact, Cancellation cancellation) const
{
  if (!artifact || destination.has_filesystem())
    throw std::invalid_argument("S3 checkpoint requires an artifact destination");
  try {
    if (mode == TransferMode::Direct)
      throw std::invalid_argument("S3 checkpoints require staging");
    store_.CheckCheckpointDestination(*artifact, cancellation);
    cancellation.ThrowIfCancelled();
    return staging_directory;
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), cancellation);
  }
}

CheckpointInventory
ModelStreamerTransferEngine::InspectCheckpoint(const Path& source, const PublishedArtifact* artifact,
    Cancellation cancellation) const
{
  if (!artifact)
    throw std::invalid_argument("S3 checkpoint requires an artifact");
  try {
    return store_.BuildCheckpointInventory(source, *artifact, cancellation);
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), cancellation);
  }
}

void
ModelStreamerTransferEngine::PublishCheckpoint(const Path& source, const StorageBackend& destination, CheckpointInventory inventory,
    CheckpointPublication* publication, Cancellation cancellation, TransferMode) const
{
  if (!publication || destination.has_filesystem())
    throw std::invalid_argument("S3 checkpoint requires an artifact destination");
  try {
    store_.PublishCheckpoint(source, std::move(inventory), *publication, cancellation);
  }
  catch (...) {
    RethrowS3TransferError(std::current_exception(), cancellation);
  }
}

}  // namespace snapshot::pagebroker
