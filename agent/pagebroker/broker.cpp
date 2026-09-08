// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "broker.hpp"

#include <sys/statvfs.h>
#include <fcntl.h>
#include <unistd.h>

#include <filesystem>
#include <array>
#include <memory>
#include <stdexcept>
#include <string>
#include <system_error>

#include "posix_copy_engine.hpp"
#include "criu_provider.h"
#include "s3_range_reader.hpp"
#include "s3_transfer.hpp"

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

struct RangeCopyContext {
  Path source;
  Path destination;
};

int
CopyRange(void* context, const char* image, uint64_t offset, uint64_t length)
{
  auto& paths = *static_cast<RangeCopyContext*>(context);
  const Path source = paths.source / image;
  const Path destination = paths.destination / image;
  const int input = open(source.c_str(), O_RDONLY | O_CLOEXEC);
  if (input < 0) return -errno;
  const int output = open(destination.c_str(), O_WRONLY | O_CREAT | O_CLOEXEC, 0600);
  if (output < 0) { const int error = -errno; close(input); return error; }
  std::array<char, 1 << 20> buffer;
  uint64_t copied = 0;
  while (copied < length) {
    const size_t count = std::min<uint64_t>(buffer.size(), length - copied);
    const ssize_t read_bytes = pread(input, buffer.data(), count, offset + copied);
    const ssize_t written = read_bytes == static_cast<ssize_t>(count) ?
        pwrite(output, buffer.data(), count, offset + copied) : -1;
    if (read_bytes != static_cast<ssize_t>(count) || written != static_cast<ssize_t>(count)) {
      const int error = errno ? -errno : -EIO;
      close(output); close(input); return error;
    }
    copied += count;
  }
  close(output); close(input); return 0;
}

void
StageDirectRestore(const Path& source, const Path& destination)
{
  fs::create_directories(destination);
  const bool s3 = S3RangeReaderEnabled();
  for (const auto& entry : fs::directory_iterator(source)) {
    if (!entry.is_regular_file() || entry.is_symlink())
      throw std::runtime_error("direct restore source contains unsupported entry");
    const std::string name = entry.path().filename();
    if (name.rfind("pages-", 0) == 0 ||
        (s3 && name != "inventory.img" && name != "manifest.yaml" &&
         name != "files.img" && name != "criu-provider_plan.json"))
      continue;
    fs::copy_file(entry.path(), destination / name, fs::copy_options::overwrite_existing);
  }
  criu_provider_plan* plan = nullptr;
  if (criu_provider_plan_load(
          (destination / "criu-provider_plan.json").c_str(), &plan) != 0)
    throw std::runtime_error("direct restore plan is unavailable");
  RangeCopyContext context{source, destination};
  const int copied = s3 ? 0 : criu_provider_plan_enumerate_source_ranges(plan, CopyRange, &context);
  criu_provider_plan_destroy(plan);
  if (copied != 0)
    throw std::runtime_error("direct restore range prefetch failed");
}

Path
TransactionDirectory(const Path& transaction_root, const std::string& transaction_id)
{
  if (!IsSafePathComponent(transaction_id))
    throw std::runtime_error("invalid transaction path component");
  return transaction_root / transaction_id;
}

}  // namespace

Broker::Broker(Path staging_root, Path storage_root) : staging_root_(fs::weakly_canonical(std::move(staging_root)))
{
  io_engines_.push_back(std::make_unique<PosixCopyEngine>(std::move(storage_root)));
  fs::remove_all(staging_root_ / "restore");
  fs::remove_all(staging_root_ / "checkpoint");
  fs::create_directories(staging_root_ / "restore");
  fs::create_directories(staging_root_ / "checkpoint");
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

    std::error_code restore_error;
    std::error_code checkpoint_error;
    fs::remove_all(TransactionDirectory(staging_root_ / "restore", id), restore_error);
    fs::remove_all(TransactionDirectory(staging_root_ / "checkpoint", id), checkpoint_error);
    if (restore_error || checkpoint_error)
      continue;
    ReleaseDirectRestoreReservation(*transaction);
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

void
Broker::ReleaseDirectRestoreReservation(Transaction& transaction)
{
  auto* descriptor = std::get_if<DirectRestoreTransactionDescriptor>(&transaction.descriptor());
  if (descriptor)
    ReleaseStaging(descriptor->TakeReservedStagingBytes());
}

Response
Broker::AbortStaging(
    const Request& request, Transaction& transaction, const Path& staging_directory, const std::exception& error)
{
  ReleaseDirectRestoreReservation(transaction);
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
  if (!request.has_request_id() || request.request_id().empty() || !request.has_transaction_id() ||
      !IsSafePathComponent(request.transaction_id()))
    return Fail(request, Failure::INVALID_REQUEST, "request and transaction IDs are required");

  Response response;
  try {
    switch (request.command_case()) {
      case Request::kStagedRestore:
        response = Restore(request);
        break;
      case Request::kPrepareStagedCheckpoint:
        response = PrepareCheckpoint(request);
        break;
      case Request::kDirectRestore:
        response = DirectRestore(request);
        break;
      case Request::kWaitDirectRestore:
        response = WaitDirectRestore(request);
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
Broker::Restore(const Request& request)
{
  const auto& operation = request.staged_restore();
  const auto& source = ValidateStagedRestore(operation);
  const auto& engine = Engine(operation.io_engine());
  return StageRestore(request, source, engine);
}

Response
Broker::DirectRestore(const Request& request)
{
  const auto& operation = request.direct_restore();
  if (!operation.has_source() || operation.source().kind_case() == StorageBackend::KIND_NOT_SET)
    throw std::invalid_argument("restore source is required");
  const auto& source = operation.source();
  if (!source.has_filesystem() || !operation.has_io_engine() || !operation.io_engine().has_posix_copy())
    throw std::invalid_argument("direct restore requires filesystem storage and POSIX I/O");
  Engine(operation.io_engine()).RestoreSize(source);
  const Path source_directory = source.filesystem().directory();
  criu_provider_plan* plan = nullptr;
  if (criu_provider_plan_load(
          (source_directory / "criu-provider_plan.json").c_str(), &plan) != 0)
    throw std::invalid_argument("direct restore plan is unavailable");
  criu_provider_requirements requirements{};
  const int requirements_status = criu_provider_plan_requirements(plan, &requirements);
  criu_provider_plan_destroy(plan);
  std::error_code plan_size_error;
  const uintmax_t plan_bytes = fs::file_size(
      source_directory / "criu-provider_plan.json", plan_size_error);
  if (requirements_status != 0 || plan_size_error ||
      requirements.stored_bytes > UINTMAX_MAX - requirements.metadata_bytes ||
      requirements.stored_bytes + requirements.metadata_bytes > UINTMAX_MAX - plan_bytes)
    throw std::invalid_argument("direct restore plan is invalid");
  const uintmax_t staging_bytes = requirements.stored_bytes + requirements.metadata_bytes + plan_bytes;
  const Path directory = TransactionDirectory(staging_root_ / "restore", request.transaction_id());
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW || fs::exists(directory))
    return Fail(request, Failure::TRANSACTION_CONFLICT, "direct restore transaction conflicts");
  if (!ReserveStaging(staging_bytes))
    return Fail(request, Failure::INSUFFICIENT_STORAGE, "insufficient tmpfs capacity");
  try {
    transaction->set_state(Transaction::State::PREPARING);
    DirectRestoreTransactionDescriptor descriptor(directory, staging_bytes);
    transaction->set_descriptor(std::move(descriptor));
    auto* stored = std::get_if<DirectRestoreTransactionDescriptor>(&transaction->descriptor());
    stored->Start([source_directory](const Path& staging) { StageDirectRestore(source_directory, staging); });
  } catch (const std::exception& error) {
    return AbortStaging(request, *transaction, directory, error);
  }
  auto response = Reply(request);
  response.mutable_direct_restore_started();
  return response;
}

Response
Broker::WaitDirectRestore(const Request& request)
{
  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction) return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  std::lock_guard lock(transaction->mutex());
  auto* descriptor = std::get_if<DirectRestoreTransactionDescriptor>(&transaction->descriptor());
  if (!descriptor || transaction->state() != Transaction::State::PREPARING)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "direct restore is not ready");
  try { descriptor->Wait(); transaction->set_state(Transaction::State::STAGED); }
  catch (const std::exception& error) { return AbortStaging(request, *transaction, descriptor->staging_directory(), error); }
  auto response = Reply(request);
  response.mutable_direct_restore_ready()->set_image_directory(descriptor->staging_directory().string());
  return response;
}

int
Broker::TakeDirectRestoreSocket(const std::string& transaction_id)
{
  auto transaction = FindTransaction(transaction_id);
  if (!transaction) return -1;
  std::lock_guard lock(transaction->mutex());
  auto* descriptor = std::get_if<DirectRestoreTransactionDescriptor>(&transaction->descriptor());
  return descriptor ? descriptor->TakeClientSocket() : -1;
}

int
Broker::TakeCheckpointSocket(const std::string& transaction_id)
{
  auto transaction = FindTransaction(transaction_id);
  if (!transaction) return -1;
  std::lock_guard lock(transaction->mutex());
  auto* descriptor = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor());
  return descriptor ? descriptor->TakeProviderSocket() : -1;
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
    transaction->set_descriptor(RestoreTransactionDescriptor(staging_directory));
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
  return StageCheckpoint(request, destination, engine, operation.external_memory_provider());
}

Response
Broker::StageCheckpoint(const Request& request, const StorageBackend& destination, const TransferEngine& engine,
                        bool external_memory_provider)
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
    CheckpointTransactionDescriptor descriptor(staging_directory, destination, engine.type());
    transaction->set_descriptor(std::move(descriptor));
    if (external_memory_provider)
      std::get<CheckpointTransactionDescriptor>(transaction->descriptor()).StartProvider();
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
  if (transaction->state() == Transaction::State::PREPARING)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "transaction is preparing");
  if (transaction->state() == Transaction::State::COMMITTED)
    return CommitSucceeded(request);

  if (const auto* restore = std::get_if<RestoreTransactionDescriptor>(&transaction->descriptor()))
    return CleanupRestore(request, *transaction, *restore);

  if (const auto* direct = std::get_if<DirectRestoreTransactionDescriptor>(&transaction->descriptor())) {
    fs::remove_all(direct->staging_directory());
    ReleaseDirectRestoreReservation(*transaction);
    transaction->clear_descriptor();
    transaction->set_state(Transaction::State::COMMITTED);
    return CommitSucceeded(request);
  }

  const auto* checkpoint = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor());
  if (checkpoint == nullptr)
    return Fail(request, Failure::INTERNAL_ERROR, "live transaction has no descriptor");
  return PublishCheckpoint(request, *transaction, *checkpoint);
}

Response
Broker::CleanupRestore(const Request& request, Transaction& transaction, const RestoreTransactionDescriptor& descriptor)
{
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
  // The sidecar accelerates only supported checkpoints. A failed index must
  // never make the ordinary CRIU checkpoint unpublishable.
  criu_provider_plan* plan = nullptr;
  if (criu_provider_plan_from_checkpoint(staging_directory.c_str(), &plan) == 0) {
    const Path path = staging_directory / "criu-provider_plan.json";
    if (criu_provider_plan_write(plan, path.c_str()) != 0)
      fs::remove(path);
    criu_provider_plan_destroy(plan);
  }
  try {
    engine.PublishCheckpoint(staging_directory, descriptor.destination_storage());
    if (S3TransferEnabled()) PublishToS3(staging_directory);
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
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() == Transaction::State::NEW || transaction->state() == Transaction::State::COMMITTED)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  if (transaction->state() == Transaction::State::ABORTED)
    return AbortSucceeded(request);

  const Path restore_root = staging_root_ / "restore";
  const Path checkpoint_root = staging_root_ / "checkpoint";
  fs::remove_all(TransactionDirectory(restore_root, request.transaction_id()));
  fs::remove_all(TransactionDirectory(checkpoint_root, request.transaction_id()));
  ReleaseDirectRestoreReservation(*transaction);
  transaction->clear_descriptor();
  transaction->set_state(Transaction::State::ABORTED);
  return AbortSucceeded(request);
}

}  // namespace snapshot::pagebroker
