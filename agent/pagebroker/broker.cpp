// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "broker.hpp"

#include <sys/statvfs.h>
#include <unistd.h>

#include <algorithm>
#include <filesystem>
#include <memory>
#include <stdexcept>
#include <string>
#include <system_error>
#include <utility>

#include "gpu/storage_manifest.hpp"
#include "gpu/checkpoint.hpp"
#include "transfer/engine/posix/posix_copy_engine.hpp"
#include "transfer/filesystem/filesystem_storage.hpp"
#include "transfer/engine/model_streamer/model_streamer_transfer_engine.hpp"
#include "transfer/restore_plan.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {

constexpr auto kGpuAbortTimeout = std::chrono::seconds{30};
constexpr auto kTerminalTransactionRetention = std::chrono::hours(1);
constexpr size_t kMaxRetainedTerminalTransactions = 1024;
// Account for a checkpoint at admission before its size is known; publication
// grows this reservation to the inspected size.
constexpr uintmax_t kCheckpointAdmissionBytes = 1;

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

const StorageBackend&
ValidateStagedRestore(const StagedRestoreRequest& request)
{
  switch (request.selector_case()) {
    case StagedRestoreRequest::kSource:
      if (request.source().kind_case() == StorageBackend::KIND_NOT_SET)
        throw std::invalid_argument("restore source is required");
      return request.source();
    case StagedRestoreRequest::kArtifact:
      // No backend serves store-bound artifacts yet.
      throw std::invalid_argument("artifact-addressed restore is not implemented");
    case StagedRestoreRequest::SELECTOR_NOT_SET:
    default:
      throw std::invalid_argument("restore source is required");
  }
}

void
ValidateArtifactMetadata(const GetArtifactMetadataRequest& request)
{
  if (request.selector_case() == GetArtifactMetadataRequest::SELECTOR_NOT_SET)
    throw std::invalid_argument("metadata subject is required");
}

const StorageBackend&
ValidateStagedCheckpoint(const PrepareStagedCheckpointRequest& request)
{
  switch (request.selector_case()) {
    case PrepareStagedCheckpointRequest::kDestination:
      if (request.destination().kind_case() == StorageBackend::KIND_NOT_SET)
        throw std::invalid_argument("checkpoint destination is required");
      return request.destination();
    case PrepareStagedCheckpointRequest::kTarget:
      // No backend serves store-bound artifacts yet.
      throw std::invalid_argument("artifact-addressed checkpoint is not implemented");
    case PrepareStagedCheckpointRequest::SELECTOR_NOT_SET:
    default:
      throw std::invalid_argument("checkpoint destination is required");
  }
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

using GpuRequest = Transaction::GpuRequest;
using GpuOperation = Transaction::GpuOperation;
using GpuState = GpuOperation::State;

void
BeginAbort(Transaction& transaction)
{
  transaction.set_state(Transaction::State::ABORTING);
  transaction.CancelWork();
}

std::string
CanonicalUuid(const std::string& input)
{
  std::string uuid;
  if (!gpu::storage::CanonicalizeGPUUUID(input, &uuid)) {
    throw std::invalid_argument("invalid GPU UUID");
  }
  return uuid;
}

GpuRequest
DecodeGpuRequest(const Request& request, std::span<const FileDescriptor> descriptors)
{
  GpuRequest decoded;
  const v1::GpuContext* context;
  const google::protobuf::RepeatedPtrField<v1::GpuTarget>* targets;
  switch (request.command_case()) {
    case Request::kCheckpointGpu:
      decoded.direction = gpu::Direction::Checkpoint;
      context = &request.checkpoint_gpu().context();
      targets = &request.checkpoint_gpu().targets();
      break;
    case Request::kRestoreGpu:
      decoded.direction = gpu::Direction::Restore;
      context = &request.restore_gpu().context();
      targets = &request.restore_gpu().targets();
      break;
    default:
      throw std::invalid_argument("expected GPU checkpoint or restore request");
  }
  decoded.captured_pids.assign(context->captured_pids().begin(), context->captured_pids().end());
  if (descriptors.size() != static_cast<size_t>(targets->size())) {
    throw std::invalid_argument("GPU request requires one pidfd per target");
  }
  for (int index = 0; index < targets->size(); ++index) {
    const auto& target = targets->Get(index);
    const int pidfd = descriptors[index].get();
    decoded.targets.push_back({target.captured_pid(), target.target_pid(), pidfd});
  }
  gpu::ValidateParticipants(decoded.captured_pids, decoded.targets);
  for (const auto& target : decoded.targets) {
    gpu::driver::ValidateTargetDescriptor(static_cast<int>(target.target_pid), target.pidfd);
  }
  for (const auto& device : context->visible_devices()) {
    decoded.visible_devices.push_back(CanonicalUuid(device));
  }
  if (decoded.visible_devices.empty()) {
    throw std::invalid_argument("GPU request requires visible devices");
  }
  for (const auto& mapping : context->device_map()) {
    decoded.device_map.push_back({CanonicalUuid(mapping.source_uuid()), CanonicalUuid(mapping.target_uuid())});
  }
  // These fields describe sets. Their wire order does not change the operation.
  std::sort(decoded.captured_pids.begin(), decoded.captured_pids.end());
  std::sort(decoded.visible_devices.begin(), decoded.visible_devices.end());
  std::ranges::sort(decoded.targets, {}, &gpu::Participant::captured_pid);
  std::ranges::sort(decoded.device_map, {}, &gpu::DeviceMapping::source_uuid);
  return decoded;
}

gpu::ArtifactPtr
CreateGpuArtifact(const Transaction& transaction, const GpuRequest& request)
{
  if (request.direction == gpu::Direction::Restore) {
    const auto* restore = std::get_if<RestoreTransactionDescriptor>(&transaction.descriptor());
    if (!restore) {
      throw std::invalid_argument("GPU restore requires a restore transaction");
    }
    return std::make_unique<gpu::Artifact>(restore->source_fd(), request.direction,
        request.captured_pids, request.visible_devices, request.device_map);
  }
  const auto* checkpoint = std::get_if<CheckpointTransactionDescriptor>(&transaction.descriptor());
  if (!checkpoint) {
    throw std::invalid_argument("GPU checkpoint requires a checkpoint transaction");
  }
  auto directory = filesystem_storage::OpenDirectory(checkpoint->staging_directory());
  return std::make_unique<gpu::Artifact>(directory.get(), request.direction,
      request.captured_pids, request.visible_devices, request.device_map);
}

Response
GpuSucceeded(const Request& request, gpu::Direction direction, const std::vector<gpu::ParticipantResult>& results)
{
  auto response = Reply(request);
  auto* complete = direction == gpu::Direction::Checkpoint
      ? response.mutable_gpu_checkpoint_complete() : response.mutable_gpu_restore_complete();
  for (const auto& participant : results) {
    auto* result = complete->add_participants();
    result->set_captured_pid(participant.captured_pid);
    result->set_bytes(participant.bytes);
  }
  return response;
}

// The engine returns only after its work is drained. Fatal cleanup exits in the
// engine, so this owner never needs to expose CUDA resource state to the broker.
class GpuCompletion {
 public:
  GpuCompletion(std::unique_lock<std::mutex>& lock, GpuOperation& operation)
      : lock_(lock), operation_(operation)
  {
    operation_.state = GpuState::Running;
    lock_.unlock();
  }

  ~GpuCompletion()
  {
    if (!lock_.owns_lock()) {
      lock_.lock();
    }
    operation_.state = outcome_;
    operation_.completed.notify_all();
  }

  void MarkRetryable()
  {
    outcome_ = GpuState::Ready;
  }

 private:
  std::unique_lock<std::mutex>& lock_;
  GpuOperation& operation_;
  GpuState outcome_ = GpuState::Finished;
};

}  // namespace

Broker::Broker(Path staging_root, Path storage_root, gpu::GpuEnginePtr gpu_engine, std::chrono::seconds lifetime)
    : lifetime_(lifetime), staging_root_(fs::weakly_canonical(std::move(staging_root))), gpu_engine_(std::move(gpu_engine))
{
  io_engines_.push_back(std::make_unique<PosixCopyEngine>(storage_root));
  io_engines_.push_back(std::make_unique<ModelStreamerTransferEngine>(std::move(storage_root)));
  filesystem_storage::RemoveStaging(staging_root_ / "restore");
  filesystem_storage::RemoveStaging(staging_root_ / "checkpoint");
  fs::create_directories(staging_root_ / "restore");
  fs::create_directories(staging_root_ / "checkpoint");
}

Broker::~Broker()
{
  CancelActiveTransactions();
}

std::vector<std::pair<std::string, Broker::TransactionHandle>>
Broker::TransactionSnapshot()
{
  std::lock_guard lock(transactions_mutex_);
  return {transactions_.begin(), transactions_.end()};
}

void
Broker::CancelActiveTransactions()
{
  std::lock_guard lock(transactions_mutex_);
  stopping_ = true;
  for (const auto& [id, transaction] : transactions_)
    transaction->CancelWork();
}

std::error_code
Broker::CleanupTransactionDirectory(const std::string&, Transaction& transaction)
{
  std::error_code error;
  transaction.CleanupTransfer(error);
  return error;
}

void
Broker::SetClock(Clock clock)
{
  now_ = std::move(clock);
}

void
Broker::ReapExpiredTransactions(std::chrono::steady_clock::time_point now)
{
  for (const auto& [id, transaction] : TransactionSnapshot()) {
    std::unique_lock transaction_lock(transaction->mutex(), std::try_to_lock);
    if (!transaction_lock.owns_lock())
      continue;
    if (transaction->state() == Transaction::State::COMMITTED || transaction->state() == Transaction::State::ABORTED) {
      // Retry missed retention and failed cleanup even without new requests.
      std::error_code error;
      transaction->CleanupTransfer(error);
      if (!error)
        RetainTerminalTransaction(id, transaction);
      continue;
    }

    if (!transaction->expired(now)) {
      continue;
    }
    transaction->ExpireTransfer(now);
    if (!CleanupExpiredTransaction(id, *transaction)) {
      continue;
    }
    std::lock_guard transactions_lock(transactions_mutex_);
    const auto current = transactions_.find(id);
    if (current != transactions_.end() && current->second == transaction) {
      transactions_.erase(current);
    }
  }
  ReapTerminalTransactions(now);
}

bool
Broker::CleanupExpiredTransaction(const std::string& id, Transaction& transaction)
{
  BeginAbort(transaction);
  if (const auto& operation = transaction.gpu_operation) {
    // The accept loop must not wait for I/O. A later sweep finishes cleanup.
    if (operation->state == GpuState::Running)
      return false;
    operation->artifact.reset();
  }
  if (CleanupTransactionDirectory(id, transaction))
    return false;
  transaction.TryClearDescriptor();
  transaction.set_state(Transaction::State::ABORTED);
  return true;
}

Broker::TransactionHandle
Broker::CreateOrGetTransaction(const std::string& transaction_id)
{
  std::lock_guard lock(transactions_mutex_);
  if (stopping_)
    throw TransferError(Failure::UNAVAILABLE, "PageBroker is shutting down");
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
  if (transaction_lock.owns_lock())
    RetainTerminalTransaction(transaction_id, transaction);
}

void
Broker::RetainTerminalTransaction(const std::string& id, const TransactionHandle& transaction)
{
  if (!transaction->retain_terminal())
    return;
  std::lock_guard terminal_lock(terminal_transactions_mutex_);
  terminal_transactions_.push_back({id, transaction, std::chrono::steady_clock::now()});
}

void
Broker::ReapTerminalTransactions(std::chrono::steady_clock::time_point now)
{
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
  // Pending cleanup retains the descriptor until RemoveStaging succeeds.
  (void)transaction.TryClearDescriptor();
  transaction.set_state(Transaction::State::ABORTED);
  std::error_code cleanup_error;
  transaction.RemoveStaging(staging_directory, cleanup_error);
  if (cleanup_error)
    return Fail(request, Failure::STORAGE_ERROR, std::string(error.what()) + "; cleanup: " + cleanup_error.message());
  return Fail(request, TransferFailureCode(error), error.what());
}

const TransferEngine&
Broker::Engine(IoEngine engine_type) const
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
    return Engine(IoEngine::POSIX_COPY);
  if (engine.has_model_streamer())
    return Engine(IoEngine::MODEL_STREAMER);
  throw std::invalid_argument("unsupported I/O engine");
}

Response
Broker::HandleRequest(const Request& request)
{
  if (request.has_capabilities() && !request.request_id().empty()) {
    auto response = Reply(request);
    response.mutable_capabilities()->set_custom_storage_available(gpu_engine_ && gpu_engine_->Available());
    return response;
  }
  if (!request.has_request_id() || request.request_id().empty() || !request.has_transaction_id() ||
      !IsSafePathComponent(request.transaction_id()))
    return Fail(request, Failure::INVALID_REQUEST, "request and transaction IDs are required");

  Response response;
  try {
    switch (request.command_case()) {
      case Request::kDirectRestore:
        response = DirectRestore(request);
        break;
      case Request::kPrepareDirectCheckpoint:
        response = PrepareDirectCheckpoint(request);
        break;
      case Request::kStagedRestore:
        response = Restore(request);
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
      case Request::kGetArtifactMetadata:
        ValidateArtifactMetadata(request.get_artifact_metadata());
        response = Fail(request, Failure::INVALID_REQUEST, "artifact metadata retrieval is not implemented");
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
    response = Fail(request, TransferFailureCode(error), error.what());
  }
  RetainTerminalTransaction(request.transaction_id());
  ReapTerminalTransactions(std::chrono::steady_clock::now());
  return response;
}



Response
Broker::DirectRestore(const Request& request)
{
  const auto& input = request.direct_restore();
  const auto& engine = Engine(input.io_engine());
  return StageRestore(request, input.source(), engine);
}

Response
Broker::Restore(const Request& request)
{
  const auto& operation = request.staged_restore();
  const auto& source = ValidateStagedRestore(operation);
  const auto& engine = Engine(operation.io_engine());
  return StageRestore(request, source, engine);
}

Response
Broker::StageRestore(const Request& request, const StorageBackend& source, const TransferEngine& engine,
      const PublishedArtifact* artifact)
{
  const Path restore_root = staging_root_ / "restore";
  const bool direct = request.has_direct_restore();
  const auto mode = direct ? TransferMode::Direct : TransferMode::Staged;
  const Path staging_directory = direct ? Path{} : TransactionDirectory(restore_root, request.transaction_id());
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (HasStagingConflict(*transaction, staging_directory, artifact != nullptr))
    return Fail(request, Failure::TRANSACTION_CONFLICT, "restore transaction conflicts");
  bool staging_reserved = false;
  bool source_prepared = false;
  uintmax_t bytes = 0;
  bool owned = false;
  RestoreAccess access;
  try {
    const bool metadata_only = request.has_get_artifact_metadata();
    transaction->PrepareTransfer(direct ? nullptr : resources_, 0, nullptr, lifetime_);
    transaction->set_state(Transaction::State::PREPARING);
    // Preparation can scan storage; it shares the transfer's deadline and cancellation.
    auto prepared = engine.PrepareRestore(source, transaction->cancellation(), artifact, metadata_only, mode);
    transaction->cancellation().ThrowIfCancelled();
    source_prepared = true;
    bytes = prepared.plan.size_bytes();
    transaction->Reserve(bytes);
    if (!direct && !ReserveStaging(bytes)) {
      std::lock_guard transactions_lock(transactions_mutex_);
      const auto current = transactions_.find(request.transaction_id());
      if (current != transactions_.end() && current->second == transaction)
        transactions_.erase(current);
      return Fail(request, Failure::INSUFFICIENT_STORAGE, "insufficient tmpfs capacity");
    }
    staging_reserved = !direct;
    if (!direct) {
      transaction->cancellation().ThrowIfCancelled();
      filesystem_storage::CreateDirectoryExclusive(staging_directory, fs::perms::all);
      owned = true;
      transaction->OwnDirectory(staging_directory);
      transaction->cancellation().ThrowIfCancelled();
    }
    access = engine.StageRestore(std::move(prepared), staging_directory, transaction->cancellation());
    // A final blocking copy may return after expiry or cancellation.
    transaction->cancellation().ThrowIfCancelled();
    if (staging_reserved)
      ReleaseStaging(bytes);
    staging_reserved = false;
    transaction->OwnDirectory(access.staging_directory);
    transaction->set_descriptor(RestoreTransactionDescriptor(access.staging_directory, std::move(access.source)));
    transaction->set_state(Transaction::State::STAGED);
  }
  catch (const std::exception& error) {
    if (staging_reserved)
      ReleaseStaging(bytes);
    if (!artifact && !source_prepared && dynamic_cast<const std::invalid_argument*>(&error)) {
      // Preserve invalid-source retries without leaving an admission reservation.
      // Waiters holding this handle see ABORTED; only this map entry is removed.
      transaction->set_state(Transaction::State::ABORTED);
      if (const auto cleanup_error = CleanupTransactionDirectory(request.transaction_id(), *transaction))
        return Fail(request, Failure::INVALID_REQUEST, std::string(error.what()) + "; cleanup: " + cleanup_error.message());
      std::lock_guard transactions_lock(transactions_mutex_);
      const auto current = transactions_.find(request.transaction_id());
      if (current != transactions_.end() && current->second == transaction)
        transactions_.erase(current);
      return Fail(request, Failure::INVALID_REQUEST, error.what());
    }
    return AbortStaging(request, *transaction, owned ? staging_directory : Path{}, error);
  }
  auto response = Reply(request);
  if (direct)
    response.mutable_direct_restore_ready();
  else if (request.has_get_artifact_metadata())
    response.mutable_get_artifact_metadata_complete()->set_manifest_directory(access.image_directory.string());
  else
    response.mutable_staged_restore_directory()->set_image_directory(access.image_directory.string());
  return response;
}

Response
Broker::PrepareCheckpoint(const Request& request)
{
  const auto& operation = request.prepare_staged_checkpoint();
  const auto& destination = ValidateStagedCheckpoint(operation);
  const auto& engine = Engine(operation.io_engine());
  return StageCheckpoint(request, destination, engine);
}

Response
Broker::StageCheckpoint(const Request& request, const StorageBackend& destination, const TransferEngine& engine,
      const PublishedArtifact* artifact)
{
  const bool direct = request.has_prepare_direct_checkpoint();
  const auto mode = direct ? TransferMode::Direct : TransferMode::Staged;
  const Path staging_directory = TransactionDirectory(staging_root_ / "checkpoint", request.transaction_id());
  Path output = staging_directory;
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW) {
    const auto* prepared = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor());
    if (!direct || transaction->state() != Transaction::State::STAGED || !prepared ||
        prepared->output() != mode || prepared->engine_type() != engine.type())
      return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint transaction conflicts");
    // Retries use the original control and never clean up an existing capture.
    transaction->cancellation().ThrowIfCancelled();
    output = engine.PrepareCheckpoint(destination, staging_directory, request.transaction_id(), mode,
        artifact, transaction->cancellation());
    transaction->cancellation().ThrowIfCancelled();
    if (prepared->staging_directory() != output)
      return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint transaction conflicts");
  } else {
    bool owned = false;
    try {
      // Admission precedes backend I/O; cleanup ownership starts only after mkdir succeeds.
      transaction->PrepareTransfer(direct ? nullptr : resources_, kCheckpointAdmissionBytes, artifact, lifetime_);
      transaction->set_state(Transaction::State::PREPARING);
      transaction->cancellation().ThrowIfCancelled();
      output = engine.PrepareCheckpoint(destination, staging_directory, request.transaction_id(), mode,
          artifact, transaction->cancellation());
      transaction->cancellation().ThrowIfCancelled();
      if (fs::exists(fs::symlink_status(output)))
        throw TransferError(Failure::TRANSACTION_CONFLICT, "checkpoint output directory already exists");
      if (transaction->has_limits() && !HasAvailableSpace(staging_root_, kCheckpointAdmissionBytes))
        throw TransferError(Failure::INSUFFICIENT_STORAGE, "insufficient checkpoint staging capacity");
      transaction->cancellation().ThrowIfCancelled();
      fs::create_directories(output.parent_path());
      transaction->cancellation().ThrowIfCancelled();
      // Publication preserves this mode: staged captures use normal directory
      // permissions filtered by umask, while direct captures remain private.
      const auto permissions = direct ? fs::perms::owner_all : fs::perms::all;
      filesystem_storage::CreateDirectoryExclusive(output, permissions);
      owned = true;
      transaction->OwnDirectory(output);
      // mkdir may have completed after the deadline or a concurrent Abort.
      transaction->cancellation().ThrowIfCancelled();
      transaction->set_descriptor(CheckpointTransactionDescriptor(output, destination, engine.type(), mode));
      transaction->set_state(Transaction::State::STAGED);
    }
    catch (const std::exception& error) {
      const bool invalid = dynamic_cast<const std::invalid_argument*>(&error) != nullptr;
      const auto* failure = dynamic_cast<const TransferError*>(&error);
      if (!artifact && !owned && (invalid || (failure && failure->code == Failure::TRANSACTION_CONFLICT))) {
        // Preserve corrected legacy requests without claiming another writer's output.
        // Waiters on this handle see ABORTED; only this map entry is removed.
        transaction->set_state(Transaction::State::ABORTED);
        if (const auto cleanup_error = CleanupTransactionDirectory(request.transaction_id(), *transaction))
          return Fail(request, invalid ? Failure::INVALID_REQUEST : failure->code,
              std::string(error.what()) + "; cleanup: " + cleanup_error.message());
        std::lock_guard transactions_lock(transactions_mutex_);
        const auto current = transactions_.find(request.transaction_id());
        if (current != transactions_.end() && current->second == transaction)
          transactions_.erase(current);
        return Fail(request, invalid ? Failure::INVALID_REQUEST : failure->code, error.what());
      }
      return AbortStaging(request, *transaction, owned ? output : Path{}, error);
    }
  }
  auto response = Reply(request);
  if (direct)
    response.mutable_direct_checkpoint_directory()->set_image_directory(output.string());
  else
    response.mutable_staged_checkpoint_directory()->set_image_directory(output.string());
  return response;
}

Response
Broker::Commit(const Request& request)
{
  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction) {
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  }
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() == Transaction::State::NEW || transaction->state() == Transaction::State::ABORTED) {
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  }
  if (transaction->expired(now_())) {
    return Fail(request, Failure::TRANSACTION_EXPIRED, "transaction expired");
  }
  if (transaction->state() == Transaction::State::PREPARING) {
    return Fail(request, Failure::TRANSACTION_CONFLICT, "transaction is preparing");
  }
  if (transaction->state() == Transaction::State::ABORTING) {
    return Fail(request, Failure::TRANSACTION_CONFLICT, "transaction is aborting");
  }
  if (transaction->state() == Transaction::State::COMMITTED) {
    std::error_code cleanup_error;
    transaction->CleanupTransfer(cleanup_error);
    return CommitSucceeded(request, transaction->publication());
  }
  if (const auto& operation = transaction->gpu_operation) {
    if (operation->state != GpuState::Finished || operation->result.has_failure()) {
      return Fail(request, Failure::TRANSACTION_CONFLICT, "GPU operation has not succeeded");
    }
    operation->artifact.reset();
  }

  if (const auto* restore = std::get_if<RestoreTransactionDescriptor>(&transaction->descriptor())) {
    return CleanupRestore(request, *transaction, *restore);
  }

  const auto* checkpoint = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor());
  if (checkpoint == nullptr) {
    return Fail(request, Failure::INTERNAL_ERROR, "live transaction has no descriptor");
  }
  return PublishCheckpoint(request, *transaction, *checkpoint);
}

Response
Broker::CleanupRestore(const Request& request, Transaction& transaction, const RestoreTransactionDescriptor& descriptor)
{
  if (!descriptor.staging_directory().empty()) {
    std::error_code error;
    transaction.RemoveStaging(descriptor.staging_directory(), error);
    if (error)
      return Fail(request, Failure::STORAGE_ERROR, "remove transaction directory: " + error.message());
  }
  // Any remaining reservation defers clearing until the reaper finishes cleanup.
  (void)transaction.TryClearDescriptor();
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
      plan = engine.InspectCheckpoint(staging_directory, publication ? &publication->artifact : nullptr, transaction.cancellation());
      transaction.Reserve(plan.size_bytes());
    }
    engine.PublishCheckpoint(staging_directory, descriptor.destination_storage(), std::move(plan), publication,
        transaction.cancellation(), descriptor.output());
    // Publication succeeded; retaining the descriptor for cleanup does not fail the commit.
    (void)transaction.TryClearDescriptor();
    transaction.set_state(Transaction::State::COMMITTED);
    std::error_code cleanup_error;
    transaction.RemoveStaging(staging_directory, cleanup_error);
  }
  catch (const std::exception& error) {
    return Fail(request, TransferFailureCode(error), error.what());
  }
  return CommitSucceeded(request, transaction.publication());
}

Response
Broker::Abort(const Request& request)
{
  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction) {
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  }
  transaction->CancelWork(); // Interrupt transfer work before waiting for its lock.
  std::unique_lock lock(transaction->mutex());
  if (transaction->state() == Transaction::State::NEW || transaction->state() == Transaction::State::COMMITTED) {
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  }
  if (transaction->state() == Transaction::State::ABORTED) {
    if (const auto error = CleanupTransactionDirectory(request.transaction_id(), *transaction))
      return Fail(request, Failure::STORAGE_ERROR, "remove transaction directory: " + error.message());
    return AbortSucceeded(request);
  }
  if (transaction->expired(now_())) {
    return Fail(request, Failure::TRANSACTION_EXPIRED, "transaction expired");
  }
  BeginAbort(*transaction);
  if (const auto operation = transaction->gpu_operation) {
    if (!operation->completed.wait_for(lock, kGpuAbortTimeout, [&] { return operation->state != GpuState::Running; })) {
      return Fail(request, Failure::INTERNAL_ERROR, "GPU cleanup is still running");
    }
    operation->artifact.reset();
  }
  if (const auto error = CleanupTransactionDirectory(request.transaction_id(), *transaction)) {
    return Fail(request, Failure::STORAGE_ERROR, "remove transaction directory: " + error.message());
  }
  transaction->TryClearDescriptor();
  transaction->set_state(Transaction::State::ABORTED);
  return AbortSucceeded(request);
}

Response
Broker::PrepareDirectCheckpoint(const Request& request)
{
  const auto& input = request.prepare_direct_checkpoint();
  const auto& engine = Engine(input.io_engine());
  return StageCheckpoint(request, input.destination(), engine);
}

Response
Broker::HandleGpuRequest(const Request& request, CancellationPtr cancellation,
                         std::vector<FileDescriptor> target_descriptors)
{
  if (request.request_id().empty() || !IsSafePathComponent(request.transaction_id()) || !cancellation) {
    return Fail(request, Failure::INVALID_REQUEST, "GPU request requires IDs and cancellation token");
  }
  try {
    auto decoded = DecodeGpuRequest(request, target_descriptors);
    if (!gpu_engine_ || !gpu_engine_->Available()) {
      return Fail(request, Failure::INVALID_REQUEST, "CustomStorage is unavailable");
    }
    auto transaction = FindTransaction(request.transaction_id());
    if (!transaction) {
      return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
    }
    std::unique_lock lock(transaction->mutex());
    if (transaction->ExpireTransfer(now_())) {
      return Fail(request, Failure::TRANSACTION_EXPIRED, "transaction expired");
    }
    if (transaction->state() != Transaction::State::STAGED) {
      return Fail(request, Failure::TRANSACTION_CONFLICT, "storage transaction is not ready");
    }
    if (const auto& previous = transaction->gpu_operation) {
      if (previous->state == GpuState::Running) {
        return Fail(request, Failure::TRANSACTION_CONFLICT, "GPU operation is running");
      }
      // A live original pidfd and equal host PID identify the same process.
      // Once it exits, do not replay a result for a reused numeric PID.
      for (const auto& target : previous->request.targets) {
        gpu::driver::ValidateTargetDescriptor(static_cast<int>(target.target_pid), target.pidfd);
      }
      if (previous->request != decoded) {
        return Fail(request, Failure::TRANSACTION_CONFLICT, "GPU operation already submitted with different inputs");
      }
      if (previous->state == GpuState::Finished) {
        auto response = previous->result;
        response.set_request_id(request.request_id());
        return response;
      }
      previous->cancellation = std::make_shared<Cancellation>(
          Cancellation::Join(transaction->cancellation(), *cancellation));
      return ExecuteGpu(request, previous, std::move(lock));
    }
    auto operation = std::make_shared<GpuOperation>();
    operation->artifact = CreateGpuArtifact(*transaction, decoded);
    operation->request = std::move(decoded);
    operation->target_descriptors = std::move(target_descriptors);
    operation->cancellation = std::make_shared<Cancellation>(
        Cancellation::Join(transaction->cancellation(), *cancellation));
    transaction->gpu_operation = operation;
    return ExecuteGpu(request, operation, std::move(lock));
  } catch (const std::invalid_argument& error) {
    return Fail(request, Failure::INVALID_REQUEST, error.what());
  } catch (const std::exception& error) {
    return Fail(request, Failure::INTERNAL_ERROR, error.what());
  }
}

Response
Broker::ExecuteGpu(const Request& request, const Transaction::GpuOperationPtr& operation,
                   std::unique_lock<std::mutex> lock)
{
  operation->result = Fail(request, Failure::INTERNAL_ERROR, "GPU operation did not return a result");
  GpuCompletion completion(lock, *operation);
  Response response;
  const auto& input = operation->request;
  try {
    std::vector<gpu::ParticipantResult> results;
    switch (input.direction) {
      case gpu::Direction::Checkpoint:
        results = gpu_engine_->Checkpoint(*operation->artifact, input.targets, *operation->cancellation);
        break;
      case gpu::Direction::Restore:
        results = gpu_engine_->Restore(*operation->artifact, input.targets, *operation->cancellation);
        break;
    }
    response = GpuSucceeded(request, input.direction, results);
  } catch (const gpu::TargetConflict& error) {
    // No CUDA work started. Keep the artifact available for this request's retry.
    completion.MarkRetryable();
    response = Fail(request, Failure::TRANSACTION_CONFLICT, error.what());
  } catch (const std::exception& error) {
    response = Fail(request, Failure::INTERNAL_ERROR, error.what());
  }
  lock.lock();
  operation->result.Swap(&response);
  return operation->result;
}

}  // namespace snapshot::pagebroker
