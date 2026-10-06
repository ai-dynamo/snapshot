// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "broker.hpp"

#include <sys/statvfs.h>
#include <sys/stat.h>
#include <fcntl.h>
#include <climits>
#include <set>
#include <unistd.h>

#include <filesystem>
#include <memory>
#include <stdexcept>
#include <string>
#include <system_error>

#include "posix_copy_engine.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {

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

}  // namespace

Broker::Broker(Path staging_root, Path storage_root, std::shared_ptr<gpu::GpuEngine> gpu_engine)
    : staging_root_(fs::weakly_canonical(std::move(staging_root))), gpu_engine_(std::move(gpu_engine))
{
  io_engines_.push_back(std::make_unique<PosixCopyEngine>(std::move(storage_root)));
  fs::remove_all(staging_root_ / "restore");
  fs::remove_all(staging_root_ / "checkpoint");
  fs::create_directories(staging_root_ / "restore");
  fs::create_directories(staging_root_ / "checkpoint");
}

Broker::~Broker() { StopGpuWork(); }

void
Broker::StopGpuWork()
{
  std::vector<TransactionHandle> transactions;
  {
    std::lock_guard lock(transactions_mutex_);
    for (const auto& [id, transaction] : transactions_) transactions.push_back(transaction);
  }
  for (const auto& transaction : transactions) {
    std::lock_guard lock(transaction->mutex());
    if (const auto& operation = transaction->gpu_operation) {
      operation->cancellation->Cancel();
    }
  }
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
    std::lock_guard transaction_lock(transaction->mutex());
    if (!transaction->expired(now, kLiveTransactionLifetime))
      continue;

    if (const auto& operation = transaction->gpu_operation) {
      transaction->set_state(Transaction::State::ABORTING);
      operation->cancellation->Cancel();
      // The transfer handler still owns the artifact until all I/O stops.
      if (operation->running) continue;
      operation->artifact.reset();
    }
    if (const auto* checkpoint = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor())) {
      std::error_code error;
      fs::remove_all(checkpoint->staging_directory(), error);
      if (error) continue;
    }
    std::error_code restore_error;
    std::error_code checkpoint_error;
    fs::remove_all(TransactionDirectory(staging_root_ / "restore", id), restore_error);
    fs::remove_all(TransactionDirectory(staging_root_ / "checkpoint", id), checkpoint_error);
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
Broker::Engine(TransferEngineType engine_type) const
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
    return Engine(TransferEngineType::POSIX_COPY);
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
      case Request::kPrepareDirectCheckpoint:
        response = PrepareDirectCheckpoint(request);
        break;
      case Request::kDirectRestore:
        response = DirectRestore(request);
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
  auto source = OpenDirectory(engine.SourceDirectory(input.source()));
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "restore transaction conflicts");
  transaction->set_state(Transaction::State::PREPARING);
  transaction->set_descriptor(RestoreTransactionDescriptor({}, std::move(source)));
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
  if (transaction->state() == Transaction::State::NEW || transaction->state() == Transaction::State::ABORTED)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  if (transaction->state() == Transaction::State::PREPARING || transaction->state() == Transaction::State::ABORTING)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "transaction is preparing");
  if (transaction->state() == Transaction::State::COMMITTED)
    return CommitSucceeded(request);

  if (const auto& gpu = transaction->gpu_operation; gpu && (!gpu->finished || gpu->result.has_failure()))
    return Fail(request, Failure::TRANSACTION_CONFLICT, "GPU operation has not succeeded");
  if (transaction->gpu_operation) transaction->gpu_operation->artifact.reset();

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
    engine.PublishCheckpoint(staging_directory, descriptor.destination_storage());
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
  if (!transaction)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  std::unique_lock lock(transaction->mutex());
  if (transaction->state() == Transaction::State::NEW || transaction->state() == Transaction::State::COMMITTED)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  if (transaction->state() == Transaction::State::ABORTED)
    return AbortSucceeded(request);

  if (const auto operation = transaction->gpu_operation) {
    transaction->set_state(Transaction::State::ABORTING);
    operation->cancellation->Cancel();
    operation->completed.wait(lock, [&] { return !operation->running; });
    operation->artifact.reset();
  }
  if (const auto* checkpoint = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor()))
    fs::remove_all(checkpoint->staging_directory());

  const Path restore_root = staging_root_ / "restore";
  const Path checkpoint_root = staging_root_ / "checkpoint";
  fs::remove_all(TransactionDirectory(restore_root, request.transaction_id()));
  fs::remove_all(TransactionDirectory(checkpoint_root, request.transaction_id()));
  transaction->clear_descriptor();
  transaction->set_state(Transaction::State::ABORTED);
  return AbortSucceeded(request);
}

Response
Broker::PrepareDirectCheckpoint(const Request& request)
{
  const auto& input = request.prepare_direct_checkpoint();
  const auto& engine = Engine(input.io_engine());
  engine.ValidateCheckpointDestination(input.destination());
  const auto destination = engine.DestinationDirectory(input.destination());
  const Path directory(destination.string() + ".pagebroker-tx-" + request.transaction_id());
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW || fs::exists(directory))
    return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint transaction conflicts");
  try {
    transaction->set_state(Transaction::State::PREPARING);
    fs::create_directories(destination.parent_path());
    if (mkdir(directory.c_str(), 0700))
      throw std::system_error(errno, std::generic_category(), "create checkpoint directory");
    transaction->set_descriptor(CheckpointTransactionDescriptor(directory, input.destination(), engine.type()));
    transaction->set_state(Transaction::State::STAGED);
  } catch (const std::exception& error) {
    return AbortStaging(request, *transaction, directory, error);
  }
  auto response = Reply(request);
  response.mutable_direct_checkpoint_directory()->set_image_directory(directory.string());
  return response;
}

Response
Broker::HandleGpuRequest(const Request& request, std::shared_ptr<Cancellation> cancellation)
{
  if (request.request_id().empty() || !IsSafePathComponent(request.transaction_id())) {
    return Fail(request, Failure::INVALID_REQUEST, "GPU request requires request and transaction IDs");
  }
  if (!gpu_engine_ || !gpu_engine_->Available()) {
    return Fail(request, Failure::INVALID_REQUEST, "CustomStorage is unavailable");
  }
  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction) { return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found"); }
  std::unique_lock lock(transaction->mutex());
  if (transaction->state() != Transaction::State::STAGED) {
    return Fail(request, Failure::TRANSACTION_CONFLICT, "storage transaction is not ready");
  }
  const bool checkpoint = request.has_checkpoint_gpu();
  const auto& context = checkpoint ? request.checkpoint_gpu().context() : request.restore_gpu().context();
  const auto& targets = checkpoint ? request.checkpoint_gpu().targets() : request.restore_gpu().targets();
  std::set<uint32_t> captured(context.captured_pids().begin(), context.captured_pids().end());
  std::set<uint32_t> target_pids;
  if (context.visible_devices().empty() || captured.empty() || captured.size() != static_cast<size_t>(context.captured_pids_size()) ||
      targets.size() != static_cast<int>(captured.size())) {
    return Fail(request, Failure::INVALID_REQUEST, "GPU context and targets must describe the same participants");
  }
  for (const auto& target : targets) {
    if (!target.captured_pid() || target.captured_pid() > INT_MAX || !captured.erase(target.captured_pid()) || !target.target_pid() || target.target_pid() > INT_MAX ||
        !target_pids.insert(target.target_pid()).second) {
      return Fail(request, Failure::INVALID_REQUEST, "invalid or duplicate GPU target");
    }
  }
  if (const auto& previous = transaction->gpu_operation) {
    if (previous->running) { return Fail(request, Failure::TRANSACTION_CONFLICT, "GPU operation is running"); }
    const auto payload = checkpoint ? request.checkpoint_gpu().SerializeAsString() : request.restore_gpu().SerializeAsString();
    if (previous->direction != (checkpoint ? gpu::Direction::Checkpoint : gpu::Direction::Restore) ||
        previous->request_payload != payload) {
      return Fail(request, Failure::TRANSACTION_CONFLICT, "GPU operation already submitted with different targets");
    }
    if (!previous->finished) {
      previous->cancellation = std::move(cancellation);
      return ExecuteGpu(request, previous, std::move(lock));
    }
    auto response = previous->result;
    response.set_request_id(request.request_id());
    return response;
  }
  std::shared_ptr<Transaction::GpuOperation> operation;
  try {
    FileDescriptor directory(-1);
    if (const auto* restore = std::get_if<RestoreTransactionDescriptor>(&transaction->descriptor()); restore && !checkpoint)
      directory = FileDescriptor(fcntl(restore->source_fd(), F_DUPFD_CLOEXEC, 0));
    else if (const auto* save = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor()); save && checkpoint)
      directory = OpenDirectory(save->staging_directory());
    else throw std::invalid_argument("GPU operation direction does not match storage transaction");
    std::vector<uint32_t> participants(context.captured_pids().begin(), context.captured_pids().end());
    std::vector<std::string> devices(context.visible_devices().begin(), context.visible_devices().end());
    std::vector<gpu::DeviceMapping> mappings;
    for (const auto& mapping : context.device_map()) mappings.push_back({mapping.source_uuid(), mapping.target_uuid()});
    operation = std::make_shared<Transaction::GpuOperation>();
    operation->cancellation = std::move(cancellation);
    operation->direction = checkpoint ? gpu::Direction::Checkpoint : gpu::Direction::Restore;
    // Validate the GPU artifact here, after CRIU on restore, not during storage preparation.
    operation->artifact = std::make_unique<gpu::Artifact>(directory.get(), operation->direction,
        std::move(participants), std::move(devices), std::move(mappings));
    transaction->gpu_operation = operation;
  } catch (const std::invalid_argument& error) { return Fail(request, Failure::INVALID_REQUEST, error.what()); }
    catch (const std::exception& error) { return Fail(request, Failure::INTERNAL_ERROR, error.what()); }
  return ExecuteGpu(request, operation, std::move(lock));
}

Response
Broker::ExecuteGpu(const Request& request, const std::shared_ptr<Transaction::GpuOperation>& operation,
                   std::unique_lock<std::mutex> lock)
{
  const bool checkpoint = request.has_checkpoint_gpu();
  const auto& input = checkpoint ? request.checkpoint_gpu().targets() : request.restore_gpu().targets();
  std::vector<gpu::Participant> targets;
  for (const auto& target : input) targets.push_back({target.captured_pid(), target.target_pid()});
  operation->request_payload = checkpoint ? request.checkpoint_gpu().SerializeAsString() : request.restore_gpu().SerializeAsString();
  auto response = Reply(request);
  auto* complete = checkpoint ? response.mutable_gpu_checkpoint_complete() : response.mutable_gpu_restore_complete();
  for (size_t index = 0; index < targets.size(); ++index) complete->add_participants();
  operation->result = Fail(request, Failure::INTERNAL_ERROR, "GPU operation did not return a result");
  struct CompletionGuard {
    std::unique_lock<std::mutex>& lock;
    Transaction::GpuOperation& operation;
    bool drained = true;
    bool retryable = false;
    ~CompletionGuard() {
      if (!drained) return;
      if (!lock.owns_lock()) lock.lock();
      operation.running = false;
      operation.finished = !retryable;
      operation.completed.notify_all();
    }
  } completion{lock, *operation};
  operation->running = true;
  lock.unlock();
  try {
    const auto results = checkpoint ? gpu_engine_->Checkpoint(*operation->artifact, targets, *operation->cancellation)
                                    : gpu_engine_->Restore(*operation->artifact, targets, *operation->cancellation);
    size_t index = 0;
    for (const auto& participant : results) {
      auto* result = complete->mutable_participants(index++);
      result->set_captured_pid(participant.captured_pid);
      result->set_bytes(participant.bytes);
    }
  } catch (const gpu::TargetConflict& error) {
    // No CUDA work started. Leave storage ready for another GPU request.
    lock.lock();
    completion.retryable = true;
    operation->result = Fail(request, Failure::TRANSACTION_CONFLICT, error.what());
    return operation->result;
  } catch (const gpu::FatalError&) {
    // The daemon handles fatal errors. Keep running=true so Abort cannot release
    // files while GPU work may still use them.
    completion.drained = false;
    throw;
  } catch (const std::exception& error) {
    response = Fail(request, Failure::INTERNAL_ERROR, error.what());
  }
  lock.lock();
  operation->result.Swap(&response);
  return operation->result;
}

}  // namespace snapshot::pagebroker
