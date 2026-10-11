// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "broker.hpp"

#include <sys/statvfs.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <unistd.h>

#include <algorithm>
#include <filesystem>
#include <memory>
#include <stdexcept>
#include <string>
#include <system_error>

#include "posix_copy_engine.hpp"
#include "gpu/storage_manifest.hpp"
#include "gpu/checkpoint.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {

constexpr auto kGpuAbortTimeout = std::chrono::seconds{30};
constexpr auto kTerminalTransactionRetention = std::chrono::hours(1);
constexpr size_t kMaxRetainedTerminalTransactions = 1024;
constexpr auto kLiveTransactionLifetime = std::chrono::hours(2) + std::chrono::minutes(5);

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
CommitSucceeded(const Request& request)
{
  auto response = Reply(request);
  response.mutable_commit_complete();
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

FileDescriptor
OpenDirectory(const Path& path)
{
  FileDescriptor directory(open(path.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW));
  if (directory.get() < 0)
    throw std::system_error(errno, std::generic_category(), "open artifact directory");
  return directory;
}

void
RejectSymlinks(const Path& directory)
{
  for (const auto& entry : fs::recursive_directory_iterator(directory)) {
    if (entry.is_symlink())
      throw std::runtime_error("checkpoint contains symlink");
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

Path
CheckpointOutputDirectory(const Path& destination, const std::string& transaction_id)
{
  return Path(destination.string() + ".pagebroker-tx-" + transaction_id);
}

void
BeginAbort(Transaction& transaction)
{
  transaction.set_state(Transaction::State::ABORTING);
  if (transaction.gpu_operation) {
    transaction.gpu_operation->cancellation->Cancel();
  }
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
        request.captured_pids, request.visible_devices, request.device_map, restore->preparation());
  }
  const auto* checkpoint = std::get_if<CheckpointTransactionDescriptor>(&transaction.descriptor());
  if (!checkpoint) {
    throw std::invalid_argument("GPU checkpoint requires a checkpoint transaction");
  }
  auto directory = OpenDirectory(checkpoint->staging_directory());
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

Broker::Broker(Path staging_root, Path storage_root, gpu::GpuEnginePtr gpu_engine)
    : staging_root_(fs::weakly_canonical(std::move(staging_root))), gpu_engine_(std::move(gpu_engine))
{
  io_engines_.push_back(std::make_unique<PosixCopyEngine>(std::move(storage_root)));
  fs::remove_all(staging_root_ / "restore");
  fs::remove_all(staging_root_ / "checkpoint");
  fs::create_directories(staging_root_ / "restore");
  fs::create_directories(staging_root_ / "checkpoint");
}

Broker::~Broker()
{
  CancelGpuWork();
}

std::vector<std::pair<std::string, Broker::TransactionHandle>>
Broker::TransactionSnapshot()
{
  std::lock_guard lock(transactions_mutex_);
  return {transactions_.begin(), transactions_.end()};
}

void
Broker::CancelGpuWork()
{
  for (const auto& [id, transaction] : TransactionSnapshot()) {
    std::lock_guard lock(transaction->mutex());
    if (transaction->gpu_operation) {
      transaction->gpu_operation->cancellation->Cancel();
    }
  }
}

std::error_code
Broker::CleanupTransactionDirectory(const std::string& id, Transaction& transaction)
{
  std::error_code error;
  if (const auto* checkpoint = std::get_if<CheckpointTransactionDescriptor>(&transaction.descriptor())) {
    fs::remove_all(checkpoint->staging_directory(), error);
    if (error) {
      return error;
    }
  }
  fs::remove_all(TransactionDirectory(staging_root_ / "restore", id), error);
  if (error) {
    return error;
  }
  fs::remove_all(TransactionDirectory(staging_root_ / "checkpoint", id), error);
  return error;
}

void
Broker::ReapExpiredTransactions(std::chrono::steady_clock::time_point now)
{
  for (const auto& [id, transaction] : TransactionSnapshot()) {
    std::lock_guard transaction_lock(transaction->mutex());
    if (!transaction->expired(now, kLiveTransactionLifetime)) {
      continue;
    }
    if (const auto& operation = transaction->gpu_operation) {
      BeginAbort(*transaction);
      // Expiry retries cleanup on a later sweep instead of waiting for I/O.
      if (operation->state == GpuState::Running) {
        continue;
      }
      operation->artifact.reset();
    }
    if (CleanupTransactionDirectory(id, *transaction)) {
      continue;
    }
    transaction->clear_descriptor();
    transaction->set_state(Transaction::State::ABORTED);

    std::lock_guard transactions_lock(transactions_mutex_);
    const auto current = transactions_.find(id);
    if (current != transactions_.end() && current->second == transaction) {
      transactions_.erase(current);
    }
  }
}

Broker::TransactionHandle
Broker::CreateOrGetTransaction(const std::string& transaction_id)
{
  std::lock_guard lock(transactions_mutex_);
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
  std::lock_guard transaction_lock(transaction->mutex());
  if (!transaction->retain_terminal())
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
  fs::remove_all(staging_directory, cleanup_error);
  if (cleanup_error)
    return Fail(request, Failure::STORAGE_ERROR, std::string(error.what()) + "; cleanup: " + cleanup_error.message());
  return Fail(request, Failure::STORAGE_ERROR, error.what());
}

const TransferEngine&
Broker::Engine(IoEngine engine_type) const
{
  for (const auto& candidate : io_engines_) {
    if (candidate->type() == engine_type)
      return *candidate;
  }
  throw std::runtime_error("configured I/O engine not found");
}

const TransferEngine&
Broker::Engine(const IOEngine& engine) const
{
  if (engine.has_posix_copy())
    return Engine(IoEngine::POSIX_COPY);
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
      default:
        response = Fail(request, Failure::INVALID_REQUEST, "unsupported operation");
        break;
    }
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

Response
Broker::DirectRestore(const Request& request)
{
  const auto& input = request.direct_restore();
  const auto& engine = Engine(input.io_engine());
  // GPU restore reads its payload through this descriptor.
  auto source = OpenDirectory(engine.SourceDirectory(input.source()));
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "restore transaction conflicts");
  auto preparation = gpu_engine_ ? gpu_engine_->PrepareRestore(source.get()) : nullptr;
  transaction->set_state(Transaction::State::PREPARING);
  transaction->set_descriptor(RestoreTransactionDescriptor({}, std::move(source), std::move(preparation)));
  transaction->set_state(Transaction::State::STAGED);
  auto response = Reply(request);
  response.mutable_direct_restore_ready();
  return response;
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
Broker::StageRestore(const Request& request, const StorageBackend& source, const TransferEngine& engine)
{
  const Path restore_root = staging_root_ / "restore";
  const Path staging_directory = TransactionDirectory(restore_root, request.transaction_id());
  const uintmax_t bytes = engine.RestoreSize(source);
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW || fs::exists(staging_directory))
    return Fail(request, Failure::TRANSACTION_CONFLICT, "restore transaction conflicts");
  if (!ReserveStaging(bytes)) {
    std::lock_guard transactions_lock(transactions_mutex_);
    const auto current = transactions_.find(request.transaction_id());
    if (current != transactions_.end() && current->second == transaction)
      transactions_.erase(current);
    return Fail(request, Failure::INSUFFICIENT_STORAGE, "insufficient tmpfs capacity");
  }
  bool staging_reserved = true;
  try {
    transaction->set_state(Transaction::State::PREPARING);
    engine.StageRestore(source, staging_directory);
    ReleaseStaging(bytes);
    staging_reserved = false;
    transaction->set_descriptor(RestoreTransactionDescriptor(staging_directory, OpenDirectory(engine.SourceDirectory(source))));
    transaction->set_state(Transaction::State::STAGED);
  }
  catch (const std::exception& error) {
    if (staging_reserved)
      ReleaseStaging(bytes);
    return AbortStaging(request, *transaction, staging_directory, error);
  }
  auto response = Reply(request);
  response.mutable_staged_restore_directory()->set_image_directory(staging_directory.string());
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
Broker::StageCheckpoint(const Request& request, const StorageBackend& destination, const TransferEngine& engine)
{
  const Path checkpoint_root = staging_root_ / "checkpoint";
  const Path staging_directory = TransactionDirectory(checkpoint_root, request.transaction_id());
  engine.ValidateCheckpointDestination(destination);
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW || fs::exists(staging_directory))
    return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint transaction conflicts");
  try {
    transaction->set_state(Transaction::State::PREPARING);
    fs::create_directory(staging_directory);
    transaction->set_descriptor(CheckpointTransactionDescriptor(staging_directory, destination, engine.type(), CheckpointOutput::Staged));
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
  if (!transaction) {
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  }
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() == Transaction::State::NEW || transaction->state() == Transaction::State::ABORTED) {
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  }
  if (transaction->state() == Transaction::State::PREPARING) {
    return Fail(request, Failure::TRANSACTION_CONFLICT, "transaction is preparing");
  }
  if (transaction->state() == Transaction::State::ABORTING) {
    return Fail(request, Failure::TRANSACTION_CONFLICT, "transaction is aborting");
  }
  if (transaction->state() == Transaction::State::COMMITTED) {
    return CommitSucceeded(request);
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
  if (!descriptor.staging_directory().empty())
    fs::remove_all(descriptor.staging_directory());
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
  if (engine.CheckpointDestinationConflicts(descriptor.destination_storage()))
    return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint destination conflicts");
  RejectSymlinks(staging_directory);
  try {
    if (descriptor.output() == CheckpointOutput::Direct) {
      engine.PromoteCheckpoint(staging_directory, descriptor.destination_storage());
    } else {
      engine.PublishCheckpoint(staging_directory, descriptor.destination_storage());
    }
    transaction.clear_descriptor();
    transaction.set_state(Transaction::State::COMMITTED);
    std::error_code cleanup_error;
    fs::remove_all(staging_directory, cleanup_error);
  }
  catch (const std::exception& error) {
    return Fail(request, Failure::STORAGE_ERROR, error.what());
  }
  return CommitSucceeded(request);
}

Response
Broker::Abort(const Request& request)
{
  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction) {
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  }
  std::unique_lock lock(transaction->mutex());
  if (transaction->state() == Transaction::State::NEW || transaction->state() == Transaction::State::COMMITTED) {
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  }
  if (transaction->state() == Transaction::State::ABORTED) {
    return AbortSucceeded(request);
  }
  if (const auto operation = transaction->gpu_operation) {
    BeginAbort(*transaction);
    if (!operation->completed.wait_for(lock, kGpuAbortTimeout, [&] { return operation->state != GpuState::Running; })) {
      return Fail(request, Failure::INTERNAL_ERROR, "GPU cleanup is still running");
    }
    operation->artifact.reset();
  }
  if (const auto error = CleanupTransactionDirectory(request.transaction_id(), *transaction)) {
    return Fail(request, Failure::STORAGE_ERROR, "remove transaction directory: " + error.message());
  }
  transaction->clear_descriptor();
  transaction->set_state(Transaction::State::ABORTED);
  return AbortSucceeded(request);
}

Response
Broker::PrepareDirectCheckpoint(const Request& request)
{
  const auto& input = request.prepare_direct_checkpoint();
  const auto& storage_engine = Engine(input.io_engine());
  storage_engine.ValidateCheckpointDestination(input.destination());
  const auto destination = storage_engine.DestinationDirectory(input.destination());
  const Path checkpoint_directory = CheckpointOutputDirectory(destination, request.transaction_id());
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW) {
    const auto* prepared = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor());
    const bool same_destination = prepared && prepared->engine_type() == storage_engine.type() &&
        storage_engine.DestinationDirectory(prepared->destination_storage()) == destination;
    if (transaction->state() != Transaction::State::STAGED || !same_destination ||
        prepared->staging_directory() != checkpoint_directory) {
      return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint transaction is already prepared");
    }
  } else {
    fs::create_directories(destination.parent_path());
    if (mkdir(checkpoint_directory.c_str(), S_IRWXU)) {
      if (errno == EEXIST) {
        return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint output directory already exists");
      }
      throw std::system_error(errno, std::generic_category(), "create checkpoint directory");
    }
    // Only this successful mkdir gives the transaction ownership for cleanup.
    try {
      transaction->set_state(Transaction::State::PREPARING);
      transaction->set_descriptor(CheckpointTransactionDescriptor(
          checkpoint_directory, input.destination(), storage_engine.type(), CheckpointOutput::Direct));
      // STAGED means the output directory is ready. Checkpoint data is written next.
      transaction->set_state(Transaction::State::STAGED);
    } catch (const std::exception& error) {
      return AbortStaging(request, *transaction, checkpoint_directory, error);
    }
  }
  auto response = Reply(request);
  response.mutable_direct_checkpoint_directory()->set_image_directory(checkpoint_directory.string());
  return response;
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
      previous->cancellation = std::move(cancellation);
      return ExecuteGpu(request, previous, std::move(lock));
    }
    auto operation = std::make_shared<GpuOperation>();
    operation->artifact = CreateGpuArtifact(*transaction, decoded);
    operation->request = std::move(decoded);
    operation->target_descriptors = std::move(target_descriptors);
    operation->cancellation = std::move(cancellation);
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
