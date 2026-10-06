// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "broker.hpp"

#include <sys/statvfs.h>

#include <filesystem>
#include <memory>
#include <stdexcept>
#include <string>
#include <system_error>
#include <utility>

#include "transfer/engine/model_streamer/model_streamer_transfer_engine.hpp"
#include "transfer/engine/posix/posix_copy_engine.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {

constexpr auto kTerminalTransactionRetention = std::chrono::hours(1);
constexpr size_t kMaxRetainedTerminalTransactions = 1024;
constexpr auto kLiveTransactionLifetime = std::chrono::hours(2) + std::chrono::minutes(5);

// The caller holds the transaction mutex while checking ownership of staging.
bool
HasStagingConflict(const Transaction& transaction, const Path& directory, bool artifact)
{
  if (transaction.state() != Transaction::State::NEW)
    return true;
  if (fs::exists(directory))
    return true;
  return artifact && fs::is_symlink(directory);
}

Response
Reply(const Request& request)
{
  Response response;
  response.set_request_id(request.request_id());
  response.set_transaction_id(request.transaction_id());
  return response;
}

Response
Fail(const Request& request, Failure::Code code, const std::string& message)
{
  auto response = Reply(request);
  response.mutable_failure()->set_code(code);
  response.mutable_failure()->set_message(message);
  return response;
}

Response
CommitSucceeded(const Request& request, const CheckpointPublication* publication = nullptr)
{
  auto response = Reply(request);
  auto* complete = response.mutable_commit_complete();
  if (publication && publication->published)
    *complete->mutable_published_artifact() = publication->artifact;
  return response;
}

Response
AbortSucceeded(const Request& request)
{
  auto response = Reply(request);
  response.mutable_abort_complete();
  return response;
}

bool
IsSafePathComponent(const std::string& value)
{
  return !value.empty() && value != "." && value != ".." && value.find('/') == std::string::npos &&
         value.find('\\') == std::string::npos && value.find('\0') == std::string::npos;
}

const StorageBackend&
ValidateStagedRestore(const StagedRestoreRequest& request)
{
  if (!request.has_source() || request.source().kind_case() == StorageBackend::KIND_NOT_SET)
    throw std::invalid_argument("restore source is required");
  return request.source();
}

const StorageBackend&
ValidateStagedCheckpoint(const PrepareStagedCheckpointRequest& request)
{
  if (!request.has_destination() || request.destination().kind_case() == StorageBackend::KIND_NOT_SET)
    throw std::invalid_argument("checkpoint destination is required");
  return request.destination();
}

bool
HasAvailableSpace(const Path& filesystem, uintmax_t required_bytes)
{
  struct statvfs stat {};
  return statvfs(filesystem.c_str(), &stat) == 0 && uintmax_t(stat.f_bavail) * stat.f_frsize >= required_bytes;
}

Path
TransactionDirectory(const Path& transaction_root, const std::string& transaction_id)
{
  if (!IsSafePathComponent(transaction_id))
    throw std::runtime_error("invalid transaction path component");
  return transaction_root / transaction_id;
}

}  // namespace

Broker::Broker(Path staging_root, Path storage_root, std::optional<S3Config> config, ModelStreamerOptions model_streamer)
    : staging_root_(fs::weakly_canonical(std::move(staging_root)))
{
  if (config) {
    resources_ = std::make_shared<TransactionResources>(config->staging_bytes, config->active_transactions, config->transaction_lifetime);
    staging_root_ /= "s3";
    io_engines_.push_back(std::make_unique<ModelStreamerTransferEngine>(std::move(*config), std::move(model_streamer)));
    // Preserve remote staging across restart; consumers may still mount it.
  } else {
    io_engines_.push_back(std::make_unique<PosixCopyEngine>(storage_root));
    io_engines_.push_back(std::make_unique<ModelStreamerTransferEngine>(std::move(storage_root), std::move(model_streamer)));
    fs::remove_all(staging_root_ / "restore");
    fs::remove_all(staging_root_ / "checkpoint");
  }
  fs::create_directories(staging_root_ / "restore");
  fs::create_directories(staging_root_ / "checkpoint");
}

void
Broker::CancelActiveTransactions()
{
  std::lock_guard lock(transactions_mutex_);
  stopping_ = true;
  for (const auto& [id, transaction] : transactions_)
    transaction->CancelTransfer();
}

void
Broker::ReapExpiredTransactions(std::chrono::steady_clock::time_point now)
{
  std::vector<std::pair<std::string, TransactionHandle>> transactions;
  {
    std::lock_guard lock(transactions_mutex_);
    for (const auto& [id, transaction] : transactions_) transactions.emplace_back(id, transaction);
  }

  for (const auto& [id, transaction] : transactions) {
    // Network operations hold the transaction lock and enforce their own
    // deadline. Reaping must not prevent the daemon from accepting Abort.
    std::unique_lock transaction_lock(transaction->mutex(), std::try_to_lock);
    if (!transaction_lock.owns_lock())
      continue;
    if (transaction->has_limits()) {
      if (transaction->expired(now, kLiveTransactionLifetime)) {
        transaction->CancelTransfer();
        if (transaction->kind() == Transaction::Kind::METADATA) {
          std::error_code error;
          transaction->CleanupTransfer(error);
          if (!error) {
            transaction->set_state(Transaction::State::ABORTED);
            std::lock_guard transactions_lock(transactions_mutex_);
            const auto current = transactions_.find(id);
            if (current != transactions_.end() && current->second == transaction)
              transactions_.erase(current);
          }
        }
      }
      continue; // Remote staging is released explicitly by its consumer.
    }
    if (!transaction->expired(now, kLiveTransactionLifetime))
      continue;

    std::error_code restore_error;
    std::error_code checkpoint_error;
    transaction->RemoveStaging(TransactionDirectory(staging_root_ / "restore", id), restore_error);
    transaction->RemoveStaging(TransactionDirectory(staging_root_ / "checkpoint", id), checkpoint_error);
    if (restore_error || checkpoint_error)
      continue;
    transaction->clear_descriptor();
    transaction->set_state(Transaction::State::ABORTED);

    std::lock_guard transactions_lock(transactions_mutex_);
    const auto current = transactions_.find(id);
    if (current != transactions_.end() && current->second == transaction)
      transactions_.erase(current);
  }
}

Broker::TransactionHandle
Broker::CreateOrGetTransaction(const std::string& transaction_id)
{
  std::lock_guard lock(transactions_mutex_);
  if (stopping_)
    throw TransferError(Failure::STORAGE_UNAVAILABLE, "PageBroker is shutting down");
  auto [iterator, inserted] = transactions_.try_emplace(transaction_id, std::make_shared<Transaction>());
  return iterator->second;
}

Broker::TransactionHandle
Broker::FindTransaction(const std::string& transaction_id)
{
  std::lock_guard lock(transactions_mutex_);
  const auto iterator = transactions_.find(transaction_id);
  return iterator == transactions_.end() ? nullptr : iterator->second;
}

void
Broker::RetainTerminalTransaction(const std::string& transaction_id)
{
  auto transaction = FindTransaction(transaction_id);
  if (!transaction)
    return;
  std::unique_lock transaction_lock(transaction->mutex(), std::try_to_lock);
  if (!transaction_lock.owns_lock() || !transaction->retain_terminal())
    return;
  std::lock_guard terminal_lock(terminal_transactions_mutex_);
  terminal_transactions_.push_back({transaction_id, std::move(transaction), std::chrono::steady_clock::now()});
}

void
Broker::ReapTerminalTransactions()
{
  const auto now = std::chrono::steady_clock::now();
  std::vector<TerminalTransaction> expired;
  {
    std::lock_guard lock(terminal_transactions_mutex_);
    while (!terminal_transactions_.empty() &&
           (now - terminal_transactions_.front().completed >= kTerminalTransactionRetention ||
            terminal_transactions_.size() > kMaxRetainedTerminalTransactions)) {
      expired.push_back(std::move(terminal_transactions_.front()));
      terminal_transactions_.pop_front();
    }
  }

  std::lock_guard lock(transactions_mutex_);
  for (const auto& item : expired) {
    const auto iterator = transactions_.find(item.id);
    if (iterator != transactions_.end() && iterator->second == item.transaction)
      transactions_.erase(iterator);
  }
}

bool
Broker::ReserveStaging(uintmax_t bytes)
{
  std::lock_guard lock(transactions_mutex_);
  if (!HasAvailableSpace(staging_root_, bytes + reserved_staging_bytes_))
    return false;
  reserved_staging_bytes_ += bytes;
  return true;
}

void
Broker::ReleaseStaging(uintmax_t bytes)
{
  std::lock_guard lock(transactions_mutex_);
  reserved_staging_bytes_ -= bytes;
}

Response
Broker::AbortStaging(
    const Request& request, Transaction& transaction, const Path& staging_directory, const std::exception& error)
{
  transaction.clear_descriptor();
  transaction.set_state(Transaction::State::ABORTED);
  std::error_code cleanup_error;
  transaction.RemoveStaging(staging_directory, cleanup_error);
  if (cleanup_error)
    return Fail(request, Failure::STORAGE_ERROR, std::string(error.what()) + "; cleanup: " + cleanup_error.message());
  const auto* transfer_error = dynamic_cast<const TransferError*>(&error);
  return Fail(request, transfer_error ? transfer_error->code : Failure::STORAGE_ERROR, error.what());
}

const TransferEngine&
Broker::Engine(TransferEngineType engine_type) const
{
  for (const auto& candidate : io_engines_) {
    if (candidate->type() == engine_type)
      return *candidate;
  }
  throw std::invalid_argument("configured I/O engine not found");
}

const TransferEngine&
Broker::Engine(const IOEngine& engine) const
{
  if (engine.has_posix_copy())
    return Engine(TransferEngineType::POSIX_COPY);
  if (engine.has_model_streamer())
    return Engine(TransferEngineType::MODEL_STREAMER);
  throw std::invalid_argument("unsupported I/O engine");
}

Response
Broker::HandleRequest(const Request& request)
{
  if (!request.has_request_id() || request.request_id().empty() || !request.has_transaction_id() ||
      !IsSafePathComponent(request.transaction_id()))
    return Fail(request, Failure::INVALID_REQUEST, "request and transaction IDs are required");

  Response response;
  try {
    switch (request.command_case()) {
      case Request::kStagedRestore:
        response = Restore(request);
        break;
      case Request::kGetArtifactMetadata:
        response = Metadata(request);
        break;
      case Request::kPrepareStagedCheckpoint:
        response = PrepareCheckpoint(request);
        break;
      case Request::kCommit:
        response = Commit(request);
        break;
      case Request::kAbort:
        response = Abort(request);
        break;
      default:
        response = Fail(request, Failure::INVALID_REQUEST, "unsupported operation");
        break;
    }
  }
  catch (const TransferError& error) {
    response = Fail(request, error.code, error.what());
  }
  catch (const std::invalid_argument& error) {
    response = Fail(request, Failure::INVALID_REQUEST, error.what());
  }
  catch (const std::exception& error) {
    response = Fail(request, Failure::STORAGE_ERROR, error.what());
  }
  RetainTerminalTransaction(request.transaction_id());
  ReapTerminalTransactions();
  return response;
}

const TransferEngine&
Broker::ArtifactEngine(const IOEngine* selection) const
{
  if (selection && !selection->has_model_streamer())
    throw std::invalid_argument("S3 artifacts require the Model Streamer engine");
  return Engine(TransferEngineType::MODEL_STREAMER);
}

Response
Broker::Metadata(const Request& request)
{
  const auto& artifact = request.get_artifact_metadata().artifact();
  const auto& engine = ArtifactEngine();
  engine.ValidateArtifact(artifact);
  return StageRestore(request, {}, engine, &artifact);
}

Response
Broker::Restore(const Request& request)
{
  const auto& operation = request.staged_restore();
  if (operation.has_artifact()) {
    if (operation.has_source())
      throw std::invalid_argument("restore cannot specify both source and artifact");
    const auto& engine = ArtifactEngine(operation.has_io_engine() ? &operation.io_engine() : nullptr);
    engine.ValidateArtifact(operation.artifact());
    return StageRestore(request, {}, engine, &operation.artifact());
  }
  const auto& source = ValidateStagedRestore(operation);
  const auto& engine = Engine(operation.io_engine());
  return StageRestore(request, source, engine);
}

Response
Broker::StageRestore(const Request& request, const StorageBackend& source, const TransferEngine& engine,
      const PublishedArtifact* artifact)
{
  const Path restore_root = staging_root_ / "restore";
  const Path staging_directory = TransactionDirectory(restore_root, request.transaction_id());
  // Reject invalid filesystem sources before claiming a transaction ID.
  RestorePlan plan;
  if (!artifact)
    plan = engine.PrepareRestore(source);
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (HasStagingConflict(*transaction, staging_directory, artifact != nullptr))
    return Fail(request, Failure::TRANSACTION_CONFLICT, "restore transaction conflicts");
  bool staging_reserved = false;
  uintmax_t bytes = 0;
  try {
    const bool metadata_only = request.has_get_artifact_metadata();
    transaction->PrepareTransfer(resources_, metadata_only ? Transaction::Kind::METADATA : Transaction::Kind::RESTORE,
        staging_directory);
    transaction->set_state(Transaction::State::PREPARING);
    if (artifact)
      plan = engine.PrepareRestore(source, transaction->control(), artifact, metadata_only);
    bytes = plan.size_bytes();
    transaction->Reserve(bytes);
    if (!ReserveStaging(bytes)) {
      std::lock_guard transactions_lock(transactions_mutex_);
      const auto current = transactions_.find(request.transaction_id());
      if (current != transactions_.end() && current->second == transaction)
        transactions_.erase(current);
      return Fail(request, Failure::INSUFFICIENT_STORAGE, "insufficient tmpfs capacity");
    }
    staging_reserved = true;
    engine.StageRestore(plan, staging_directory, transaction->control());
    ReleaseStaging(bytes);
    staging_reserved = false;
    transaction->set_descriptor(RestoreTransactionDescriptor(staging_directory));
    transaction->set_state(Transaction::State::STAGED);
  }
  catch (const std::exception& error) {
    if (staging_reserved)
      ReleaseStaging(bytes);
    return AbortStaging(request, *transaction, staging_directory, error);
  }
  auto response = Reply(request);
  if (request.has_get_artifact_metadata())
    response.mutable_get_artifact_metadata_complete()->set_manifest_directory(staging_directory.string());
  else
    response.mutable_staged_restore_directory()->set_image_directory(staging_directory.string());
  return response;
}

Response
Broker::PrepareCheckpoint(const Request& request)
{
  const auto& operation = request.prepare_staged_checkpoint();
  if (operation.has_target()) {
    if (operation.has_destination())
      throw std::invalid_argument("checkpoint cannot specify both destination and target");
    const auto& engine = ArtifactEngine(operation.has_io_engine() ? &operation.io_engine() : nullptr);
    const auto artifact = engine.ResolveArtifactTarget(operation.target());
    return StageCheckpoint(request, {}, engine, &artifact);
  }
  const auto& destination = ValidateStagedCheckpoint(operation);
  const auto& engine = Engine(operation.io_engine());
  return StageCheckpoint(request, destination, engine);
}

Response
Broker::StageCheckpoint(const Request& request, const StorageBackend& destination, const TransferEngine& engine,
      const PublishedArtifact* artifact)
{
  const Path checkpoint_root = staging_root_ / "checkpoint";
  const Path staging_directory = TransactionDirectory(checkpoint_root, request.transaction_id());
  if (!artifact)
    engine.ValidateCheckpointDestination(destination);
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (HasStagingConflict(*transaction, staging_directory, artifact != nullptr))
    return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint transaction conflicts");
  try {
    transaction->PrepareTransfer(resources_, Transaction::Kind::CHECKPOINT, staging_directory, artifact);
    transaction->set_state(Transaction::State::PREPARING);
    if (transaction->has_limits()) {
      if (!HasAvailableSpace(staging_root_, 1))
        throw TransferError(Failure::INSUFFICIENT_STORAGE, "insufficient checkpoint staging capacity");
      engine.ValidateCheckpointDestination(destination, artifact, transaction->control());
    }
    fs::create_directory(staging_directory);
    if (transaction->has_limits())
      fs::permissions(staging_directory, fs::perms::owner_all);
    transaction->set_descriptor(CheckpointTransactionDescriptor(staging_directory, destination, engine.type()));
    transaction->set_state(Transaction::State::STAGED);
  }
  catch (const std::exception& error) {
    return AbortStaging(request, *transaction, staging_directory, error);
  }
  auto response = Reply(request);
  response.mutable_staged_checkpoint_directory()->set_image_directory(staging_directory.string());
  return response;
}

Response
Broker::Commit(const Request& request)
{
  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  std::lock_guard lock(transaction->mutex());
  if (transaction->ReleaseExpiredTransfer())
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction expired");
  if (transaction->state() == Transaction::State::NEW || transaction->state() == Transaction::State::ABORTED)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  if (transaction->state() == Transaction::State::PREPARING)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "transaction is preparing");
  if (transaction->state() == Transaction::State::COMMITTED) {
    std::error_code cleanup_error;
    transaction->CleanupTransfer(cleanup_error); // Publication already succeeded.
    return CommitSucceeded(request, transaction->publication());
  }

  if (const auto* restore = std::get_if<RestoreTransactionDescriptor>(&transaction->descriptor()))
    return CleanupRestore(request, *transaction, *restore);

  const auto* checkpoint = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor());
  if (checkpoint == nullptr)
    return Fail(request, Failure::INTERNAL_ERROR, "live transaction has no descriptor");
  return PublishCheckpoint(request, *transaction, *checkpoint);
}

Response
Broker::CleanupRestore(const Request& request, Transaction& transaction, const RestoreTransactionDescriptor& descriptor)
{
  transaction.RemoveStaging(descriptor.staging_directory());
  transaction.clear_descriptor();
  transaction.set_state(Transaction::State::COMMITTED);
  return CommitSucceeded(request);
}

Response
Broker::PublishCheckpoint(
    const Request& request, Transaction& transaction, const CheckpointTransactionDescriptor& descriptor)
{
  const Path staging_directory = descriptor.staging_directory();
  if (!fs::is_directory(staging_directory))
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "checkpoint staging directory not found");
  const auto& engine = Engine(descriptor.engine_type());
  try {
    auto* publication = transaction.publication();
    RestorePlan plan;
    if (!publication || publication->pending_index.empty()) {
      plan = engine.InspectCheckpoint(staging_directory, publication ? &publication->artifact : nullptr, transaction.control());
      transaction.Reserve(plan.size_bytes());
    }
    engine.PublishCheckpoint(staging_directory, descriptor.destination_storage(), std::move(plan), publication, transaction.control());
    transaction.clear_descriptor();
    transaction.set_state(Transaction::State::COMMITTED);
    std::error_code cleanup_error;
    transaction.RemoveStaging(staging_directory, cleanup_error);
  }
  catch (const TransferError& error) {
    return Fail(request, error.code, error.what());
  }
  catch (const std::exception& error) {
    return Fail(request, Failure::STORAGE_ERROR, error.what());
  }
  return CommitSucceeded(request, transaction.publication());
}

Response
Broker::Abort(const Request& request)
{
  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  transaction->CancelTransfer(); // Signal the in-flight operation before waiting for its lock.
  std::lock_guard lock(transaction->mutex());
  if (transaction->ReleaseExpiredTransfer())
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction expired");
  if (transaction->state() == Transaction::State::NEW || transaction->state() == Transaction::State::COMMITTED)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  if (transaction->state() == Transaction::State::ABORTED) {
    transaction->CleanupTransfer();
    return AbortSucceeded(request);
  }

  const Path restore_root = staging_root_ / "restore";
  const Path checkpoint_root = staging_root_ / "checkpoint";
  transaction->RemoveStaging(TransactionDirectory(restore_root, request.transaction_id()));
  transaction->RemoveStaging(TransactionDirectory(checkpoint_root, request.transaction_id()));
  transaction->clear_descriptor();
  transaction->set_state(Transaction::State::ABORTED);
  return AbortSucceeded(request);
}

}  // namespace snapshot::pagebroker
