// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "broker.hpp"

#include <sys/resource.h>
#include <sys/statvfs.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <cstdlib>
#include <filesystem>
#include <cstdio>
#include <limits>
#include <memory>
#include <stdexcept>
#include <string>
#include <string_view>
#include <system_error>

#include "posix_copy_engine.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {

constexpr auto kTerminalTransactionRetention = std::chrono::hours(1);
constexpr size_t kMaxRetainedTerminalTransactions = 1024;
constexpr auto kStageReadyMarkerRetention = std::chrono::hours(1);
// Transactions that have not crossed restore admission or CUDA dispatch are
// safe to reap independently: no workload process can have been mutated.
constexpr auto kPreMutationTransactionLifetime = std::chrono::minutes(10);
constexpr uint32_t kMaximumCudaTargets = 64;
constexpr std::string_view kCudaJobFileName = "cuda-checkpoint-job";
constexpr size_t kMinimumDescriptorHeadroom = 256;
constexpr size_t kDescriptorBudgetCeiling = 1 << 20;
constexpr size_t kMaximumRestoreIdentityFieldBytes = 253;

size_t
DirectRestoreDescriptorBudget()
{
  rlimit limit{};
  if (getrlimit(RLIMIT_NOFILE, &limit) != 0)
    throw std::system_error(
        errno, std::generic_category(), "get PageBroker file descriptor limit");
  const size_t current =
      limit.rlim_cur == RLIM_INFINITY
          ? kDescriptorBudgetCeiling
          : static_cast<size_t>(std::min<rlim_t>(
                limit.rlim_cur, static_cast<rlim_t>(kDescriptorBudgetCeiling)));
  const size_t headroom = std::max(kMinimumDescriptorHeadroom, current / 4);
  return current > headroom ? current - headroom : 0;
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

std::string JsonEscape(std::string_view value)
{
  std::string result;
  result.reserve(value.size());
  constexpr char hex[] = "0123456789abcdef";
  for (const unsigned char byte : value) {
    switch (byte) {
      case '"': result += "\\\""; break;
      case '\\': result += "\\\\"; break;
      case '\b': result += "\\b"; break;
      case '\f': result += "\\f"; break;
      case '\n': result += "\\n"; break;
      case '\r': result += "\\r"; break;
      case '\t': result += "\\t"; break;
      default:
        if (byte < 0x20) {
          result += "\\u00";
          result.push_back(hex[byte >> 4]);
          result.push_back(hex[byte & 0x0f]);
        }
        else {
          result.push_back(static_cast<char>(byte));
        }
    }
  }
  return result;
}

Response
FailCuda(const Request& request, const CudaOperationResult& result)
{
  auto response = Fail(request, result.failure_code, result.error);
  response.mutable_failure()->set_target_may_be_mutated(result.target_may_be_mutated);
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

bool
IsValidUtf8(std::string_view value)
{
  const auto continuation = [](unsigned char byte) {
    return byte >= 0x80 && byte <= 0xbf;
  };
  for (size_t index = 0; index < value.size();) {
    const auto byte = static_cast<unsigned char>(value[index]);
    if (byte <= 0x7f) {
      ++index;
      continue;
    }
    if (byte >= 0xc2 && byte <= 0xdf && index + 1 < value.size() &&
        continuation(static_cast<unsigned char>(value[index + 1]))) {
      index += 2;
      continue;
    }
    if (index + 2 < value.size()) {
      const auto second = static_cast<unsigned char>(value[index + 1]);
      const auto third = static_cast<unsigned char>(value[index + 2]);
      if (((byte == 0xe0 && second >= 0xa0 && second <= 0xbf) ||
           ((byte >= 0xe1 && byte <= 0xec) && continuation(second)) ||
           (byte == 0xed && second >= 0x80 && second <= 0x9f) ||
           ((byte >= 0xee && byte <= 0xef) && continuation(second))) &&
          continuation(third)) {
        index += 3;
        continue;
      }
    }
    if (index + 3 < value.size()) {
      const auto second = static_cast<unsigned char>(value[index + 1]);
      const auto third = static_cast<unsigned char>(value[index + 2]);
      const auto fourth = static_cast<unsigned char>(value[index + 3]);
      if (((byte == 0xf0 && second >= 0x90 && second <= 0xbf) ||
           ((byte >= 0xf1 && byte <= 0xf3) && continuation(second)) ||
           (byte == 0xf4 && second >= 0x80 && second <= 0x8f)) &&
          continuation(third) && continuation(fourth)) {
        index += 4;
        continue;
      }
    }
    return false;
  }
  return true;
}

void
ValidateRestoreIdentityField(std::string_view value, const char* label)
{
  if (value.empty())
    throw std::invalid_argument(std::string("restore ") + label +
                                " is required");
  if (value.size() > kMaximumRestoreIdentityFieldBytes)
    throw std::invalid_argument(
        std::string("restore ") + label + " exceeds " +
        std::to_string(kMaximumRestoreIdentityFieldBytes) + " bytes");
  if (!IsValidUtf8(value))
    throw std::invalid_argument(std::string("restore ") + label +
                                " is not valid UTF-8");
  for (const unsigned char byte : value) {
    if (byte < 0x20 || byte == 0x7f)
      throw std::invalid_argument(
          std::string("restore ") + label +
          " contains a control character");
  }
}

bool
SameRestoreIdentity(const RestoreIdentity& left, const RestoreIdentity& right)
{
  // Do not use protobuf serialization as an identity primitive: wire bytes
  // can differ for semantically identical messages and unknown fields must
  // not silently become part of this authorization boundary.
  return left.has_pod_uid() == right.has_pod_uid() &&
         left.pod_uid() == right.pod_uid() &&
         left.has_destination_container() ==
             right.has_destination_container() &&
         left.destination_container() == right.destination_container() &&
         left.has_content_uid() == right.has_content_uid() &&
         left.content_uid() == right.content_uid() &&
         left.has_source_container() == right.has_source_container() &&
         left.source_container() == right.source_container() &&
         left.has_container_id() == right.has_container_id() &&
         left.container_id() == right.container_id();
}

void
ValidateRestoreIdentity(const RestoreIdentity& identity)
{
  if (!identity.has_pod_uid() || !identity.has_destination_container() ||
      !identity.has_content_uid() || !identity.has_source_container() ||
      !identity.has_container_id())
    throw std::invalid_argument("restore identity fields are required");
  ValidateRestoreIdentityField(identity.pod_uid(), "pod UID");
  ValidateRestoreIdentityField(identity.destination_container(),
                               "destination container");
  ValidateRestoreIdentityField(identity.content_uid(), "content UID");
  ValidateRestoreIdentityField(identity.source_container(),
                               "source container");
  ValidateRestoreIdentityField(identity.container_id(), "container ID");
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

Path
TransactionDirectory(const Path& transaction_root, const std::string& transaction_id)
{
  if (!IsSafePathComponent(transaction_id))
    throw std::runtime_error("invalid transaction path component");
  return transaction_root / transaction_id;
}

std::string
ReferenceOwner(const Path& staging_root)
{
  const char* release = std::getenv("PAGEBROKER_RELEASE_ID");
  const char* node = std::getenv("NODE_NAME");
  if (release != nullptr && *release != '\0' && node != nullptr &&
      *node != '\0')
    return std::string(release) + "\n" + node;
  // Standalone/test brokers must not sweep another process's references when
  // the production owner identity is unavailable. They sacrifice crash GC,
  // but normal transaction cleanup remains exact.
  return staging_root.string() + "\nstandalone:" +
         std::to_string(getpid());
}

std::string
StageReadyNode(const Path& staging_root)
{
  const char* node = std::getenv("NODE_NAME");
  if (node != nullptr && *node != '\0')
    return node;
  return "standalone:" + std::to_string(getpid()) + ":" +
         staging_root.string();
}

}  // namespace

Broker::Broker(
    Path staging_root,
    Path storage_root,
    uintmax_t max_staging_bytes,
    std::unique_ptr<CudaEngine> cuda_engine,
    std::unique_ptr<TransferEngine> transfer_engine,
    std::optional<size_t> direct_restore_descriptor_budget,
    StageReadyGate::FailureForTesting stage_ready_failure_for_testing)
    : staging_root_(fs::weakly_canonical(std::move(staging_root))),
      stage_ready_gate_(storage_root, ReferenceOwner(staging_root_),
                        StageReadyNode(staging_root_),
                        std::move(stage_ready_failure_for_testing)),
      max_staging_bytes_(max_staging_bytes),
      cuda_engine_(std::move(cuda_engine)),
      direct_restore_descriptor_budget_(
          direct_restore_descriptor_budget.value_or(
              DirectRestoreDescriptorBudget()))
{
  if (max_staging_bytes_ == 0)
    throw std::invalid_argument("maximum staging capacity must be positive");
  if (transfer_engine != nullptr)
    io_engines_.push_back(std::move(transfer_engine));
  else
    io_engines_.push_back(std::make_unique<PosixCopyEngine>(
        std::move(storage_root), ReferenceOwner(staging_root_)));
  fs::remove_all(staging_root_ / "restore");
  fs::remove_all(staging_root_ / "checkpoint");
  fs::create_directories(staging_root_ / "restore");
  fs::create_directories(staging_root_ / "checkpoint");
}

bool
Broker::ReapRetainedStageReadyMarkers(
    std::chrono::system_clock::time_point now, std::string* error)
{
  try {
    stage_ready_gate_.ReapRetainedMarkers(now, kStageReadyMarkerRetention);
    if (error != nullptr)
      error->clear();
    return true;
  }
  catch (const std::exception& reap_error) {
    if (error != nullptr)
      *error = reap_error.what();
    return false;
  }
  catch (...) {
    if (error != nullptr)
      *error = "unknown stage-ready marker maintenance failure";
    return false;
  }
}

bool
Broker::ReapExitedCuda(std::string* error)
{
  if (cuda_engine_ != nullptr) {
    const bool reaped = cuda_engine_->ReapExited(error);
    // Cleanup can succeed after a worker generation is lost, but that does
    // not make the fixed-size pool reusable. Always honor the engine health
    // signal so PageBroker cannot remain Ready with permanent CUDA admission
    // loss.
    if (cuda_engine_->ShutdownRequired()) {
      cuda_engine_->FailStop();
      shutdown_requested_.store(true, std::memory_order_release);
    }
    return reaped;
  }
  if (error != nullptr)
    error->clear();
  return true;
}

bool
Broker::BeginShutdownCuda(std::string* error)
{
  try {
    stage_ready_gate_.BeginShutdown();
  }
  catch (const std::exception& stage_error) {
    if (error != nullptr)
      *error = std::string("close PageBroker restore activation gate: ") +
               stage_error.what();
    return false;
  }
  if (cuda_engine_ != nullptr)
    return cuda_engine_->BeginShutdown(error);
  if (error != nullptr)
    error->clear();
  return true;
}

bool
Broker::ShutdownCuda(std::string* error)
{
  if (cuda_engine_ != nullptr)
    return cuda_engine_->Shutdown(error);
  if (error != nullptr)
    error->clear();
  return true;
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
    // Request handlers hold this mutex across staging and CUDA operations.
    // Maintenance runs on the daemon's accept thread, so waiting here would
    // stop unrelated admission and delay SIGTERM-driven CUDA shutdown behind
    // the longest active transaction. A busy transaction cannot be safely
    // inspected yet and will be reconsidered on the next maintenance pass.
    std::unique_lock transaction_lock(
        transaction->mutex(), std::try_to_lock);
    if (!transaction_lock.owns_lock())
      continue;
    if (!transaction->expired(now, kPreMutationTransactionLifetime))
      continue;

    Path restore_directory =
        TransactionDirectory(staging_root_ / "restore", id);
    if (const auto* descriptor =
            std::get_if<RestoreTransactionDescriptor>(
                &transaction->descriptor()))
      restore_directory = descriptor->staging_directory();
    std::error_code restore_error;
    std::error_code checkpoint_error;
    fs::remove_all(restore_directory, restore_error);
    fs::remove_all(TransactionDirectory(staging_root_ / "checkpoint", id), checkpoint_error);
    if (restore_error || checkpoint_error)
      continue;
    ReleaseStaging(*transaction);
    transaction->clear_restore_admission();
    transaction->clear_descriptor();
    ReleaseDirectRestoreDescriptors(*transaction);
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
Broker::ReserveStaging(uintmax_t bytes, bool copy_pending)
{
  std::lock_guard lock(staging_capacity_mutex_);
  if (bytes > max_staging_bytes_ - reserved_staging_bytes_ ||
      bytes > std::numeric_limits<uintmax_t>::max() - pending_copy_bytes_ ||
      !HasAvailableSpace(staging_root_, bytes + pending_copy_bytes_))
    return false;
  reserved_staging_bytes_ += bytes;
  if (copy_pending)
    pending_copy_bytes_ += bytes;
  return true;
}

void
Broker::FinishStagingCopy(uintmax_t bytes)
{
  std::lock_guard lock(staging_capacity_mutex_);
  if (bytes > pending_copy_bytes_)
    throw std::logic_error("PageBroker staging copy reservation underflow");
  pending_copy_bytes_ -= bytes;
}

void
Broker::ReleaseStaging(Transaction& transaction)
{
  const uintmax_t bytes = transaction.staging_reservation_bytes();
  if (bytes == 0)
    return;
  std::lock_guard lock(staging_capacity_mutex_);
  if (bytes > reserved_staging_bytes_)
    throw std::logic_error("PageBroker staging reservation underflow");
  reserved_staging_bytes_ -= bytes;
  transaction.set_staging_reservation_bytes(0);
}

bool
Broker::ReserveDirectRestoreDescriptors(size_t descriptors)
{
  std::lock_guard lock(direct_restore_descriptor_mutex_);
  if (descriptors == 0 ||
      descriptors > direct_restore_descriptor_budget_ -
                        reserved_direct_restore_descriptors_)
    return false;
  reserved_direct_restore_descriptors_ += descriptors;
  return true;
}

void
Broker::ReleaseDirectRestoreDescriptors(Transaction& transaction)
{
  const size_t descriptors =
      transaction.direct_restore_descriptor_reservation();
  if (descriptors == 0)
    return;
  std::lock_guard lock(direct_restore_descriptor_mutex_);
  if (descriptors > reserved_direct_restore_descriptors_)
    throw std::logic_error(
        "PageBroker direct restore descriptor reservation underflow");
  reserved_direct_restore_descriptors_ -= descriptors;
  transaction.set_direct_restore_descriptor_reservation(0);
}

bool
Broker::CleanupStaging(Transaction& transaction, const Path& staging_directory, std::error_code& error)
{
  fs::remove_all(staging_directory, error);
  if (error)
    return false;
  ReleaseStaging(transaction);
  return true;
}

Response
Broker::AbortStaging(
    const Request& request, Transaction& transaction,
    const Path& staging_directory, const std::exception& error,
    Failure::Code code)
{
  std::error_code cleanup_error;
  if (!CleanupStaging(transaction, staging_directory, cleanup_error))
    return Fail(request, Failure::STORAGE_ERROR, std::string(error.what()) + "; cleanup: " + cleanup_error.message());
  transaction.clear_descriptor();
  ReleaseDirectRestoreDescriptors(transaction);
  transaction.set_state(Transaction::State::ABORTED);
  return Fail(request, code, error.what());
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
  const bool cuda_command =
      request.command_case() == Request::kCudaCheckpoint ||
      request.command_case() == Request::kCudaRestore;
  const auto fail_unexpected_cuda = [&](const std::string& message) {
    if (cuda_engine_)
      cuda_engine_->FailStop();
    shutdown_requested_.store(true, std::memory_order_release);
    return FailCuda(request, {
      .target_may_be_mutated = true,
      .fatal = true,
      .error = message,
    });
  };
  try {
    switch (request.command_case()) {
      case Request::kStagedRestore:
        response = Restore(request);
        break;
      case Request::kReferenceRegularRestore:
        response = ReferenceRegularRestore(request);
        break;
      case Request::kDirectRestore:
        response = DirectRestore(request);
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
      case Request::kBeginRestore:
        response = BeginRestore(request);
        break;
      case Request::kActivateRestore:
        response = ActivateRestore(request);
        break;
      case Request::kBeginCheckpoint:
        response = BeginCheckpoint(request);
        break;
      case Request::kCudaCheckpoint:
        response = CudaCheckpoint(request);
        break;
      case Request::kCudaRestore:
        response = CudaRestore(request);
        break;
      default:
        response = Fail(request, Failure::INVALID_REQUEST, "unsupported operation");
        break;
    }
  }
  catch (const std::invalid_argument& error) {
    if (cuda_command)
      response = fail_unexpected_cuda(
          std::string("unexpected CUDA command failure: ") + error.what());
    else
      response = Fail(request, Failure::INVALID_REQUEST, error.what());
  }
  catch (const std::exception& error) {
    if (cuda_command)
      response = fail_unexpected_cuda(
          std::string("unexpected CUDA command failure: ") + error.what());
    else
      response = Fail(request, Failure::STORAGE_ERROR, error.what());
  }
  catch (...) {
    if (!cuda_command)
      throw;
    response = fail_unexpected_cuda("unexpected CUDA command failure");
  }
  RetainTerminalTransaction(request.transaction_id());
  ReapTerminalTransactions();
  return response;
}

Response
Broker::CudaCheckpoint(const Request& request)
{
  if (!cuda_engine_)
    return Fail(request, Failure::INVALID_REQUEST, "PageBroker CUDA engine is disabled");
  return ExecuteCudaCheckpoint(request);
}

Response
Broker::CudaRestore(const Request& request)
{
  if (!cuda_engine_)
    return Fail(request, Failure::INVALID_REQUEST, "PageBroker CUDA engine is disabled");
  return ExecuteCudaRestore(request);
}

Response
Broker::ExecuteCudaCheckpoint(const Request& request)
{
  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::CHECKPOINT_ADMITTED ||
      !transaction->has_checkpoint_admission())
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "CUDA checkpoint requires prior checkpoint admission");

  const Path* staging_directory = nullptr;
  if (const auto* descriptor = std::get_if<CheckpointTransactionDescriptor>(&transaction->descriptor()))
    staging_directory = &descriptor->staging_directory();
  if (staging_directory == nullptr)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "CUDA operation does not match transaction kind");
  if (!request.cuda_checkpoint().has_storage_backend() ||
      transaction->checkpoint_backend() !=
          request.cuda_checkpoint().storage_backend())
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "CUDA checkpoint backend does not match admission");
  if (transaction->checkpoint_target_count() !=
      static_cast<size_t>(request.cuda_checkpoint().targets_size()))
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "CUDA checkpoint target count does not match admission");

  CudaOperationResult result;
  auto admission = transaction->take_checkpoint_admission();
  transaction->set_state(Transaction::State::CUDA_STARTED);
  try {
    result = cuda_engine_->Checkpoint(request.cuda_checkpoint(),
                                      *staging_directory, *admission);
  }
  catch (const std::invalid_argument&) {
    // CUDA request validation happens before the first driver call.
    throw;
  }
  catch (const std::exception& error) {
    // Once execution begins, an unexpected failure cannot prove that the
    // target was not mutated. Preserve the CUDA failure classification so the
    // agent fails closed and tears down the source/placeholder.
    result.target_may_be_mutated = true;
    result.fatal = true;
    result.error = std::string("unexpected CUDA engine failure: ") + error.what();
  }
  catch (...) {
    result.target_may_be_mutated = true;
    result.fatal = true;
    result.error = "unexpected CUDA engine failure";
  }
  if (result.fatal) {
    cuda_engine_->FailStop();
    shutdown_requested_.store(true, std::memory_order_release);
  }
  if (!result.succeeded)
    return FailCuda(request, result);
  transaction->set_state(Transaction::State::CUDA_COMPLETE);
  auto response = Reply(request);
  auto *complete = response.mutable_cuda_operation_complete();
  complete->set_target_count(result.target_count);
  if (result.has_cuinterpose_state) {
    auto *state = complete->mutable_cuinterpose_state();
    state->set_protocol_version(result.cuinterpose_protocol_version);
    state->set_size_bytes(result.cuinterpose_state_size);
    state->set_sha256(result.cuinterpose_state_sha256);
    state->set_participant_count(result.cuinterpose_participant_count);
  }
  return response;
}

Response
Broker::BeginCheckpoint(const Request& request)
{
  if (!cuda_engine_)
    return Fail(request, Failure::INVALID_REQUEST, "PageBroker CUDA engine is disabled");
  const auto& operation = request.begin_checkpoint();
  if (!operation.has_storage_backend() ||
      operation.storage_backend() == v1::CUDA_STORAGE_BACKEND_UNSPECIFIED)
    return Fail(request, Failure::INVALID_REQUEST,
                "checkpoint admission requires a CUDA storage backend");
  if (!operation.has_target_count() || operation.target_count() == 0 ||
      operation.target_count() > kMaximumCudaTargets)
    return Fail(request, Failure::INVALID_REQUEST,
                "checkpoint admission requires between 1 and 64 CUDA targets");

  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND,
                "transaction not found");
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() == Transaction::State::CHECKPOINT_ADMITTED) {
    if (transaction->checkpoint_backend() == operation.storage_backend() &&
        transaction->checkpoint_target_count() == operation.target_count() &&
        transaction->has_checkpoint_admission()) {
      auto response = Reply(request);
      response.mutable_checkpoint_admission_granted();
      return response;
    }
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "checkpoint admission backend or target count conflicts");
  }
  if (transaction->state() != Transaction::State::STAGED ||
      std::get_if<CheckpointTransactionDescriptor>(
          &transaction->descriptor()) == nullptr)
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "checkpoint admission requires a staged checkpoint transaction");

  CheckpointAdmissionResult result;
  try {
    result = cuda_engine_->BeginCheckpoint(operation.storage_backend(),
                                           operation.target_count());
  }
  catch (const std::invalid_argument& error) {
    return Fail(request, Failure::INVALID_REQUEST, error.what());
  }
  catch (const std::exception& error) {
    result.operation.target_may_be_mutated = false;
    result.operation.fatal = true;
    result.operation.error =
        std::string("unexpected CUDA checkpoint admission failure: ") +
        error.what();
  }
  catch (...) {
    result.operation.target_may_be_mutated = false;
    result.operation.fatal = true;
    result.operation.error =
        "unexpected CUDA checkpoint admission failure";
  }
  if (result.operation.fatal) {
    cuda_engine_->FailStop();
    shutdown_requested_.store(true, std::memory_order_release);
  }
  if (result.admission == nullptr)
    return FailCuda(request, result.operation);

  transaction->set_checkpoint_admission(
      std::move(result.admission), operation.storage_backend(),
      operation.target_count());
  transaction->set_state(Transaction::State::CHECKPOINT_ADMITTED);
  auto response = Reply(request);
  response.mutable_checkpoint_admission_granted();
  return response;
}

Response
Broker::BeginRestore(const Request& request)
{
  if (!cuda_engine_)
    return Fail(request, Failure::INVALID_REQUEST, "PageBroker CUDA engine is disabled");
  const auto& operation = request.begin_restore();
  if (!operation.has_storage_backend() ||
      operation.storage_backend() == v1::CUDA_STORAGE_BACKEND_UNSPECIFIED)
    return Fail(request, Failure::INVALID_REQUEST, "restore admission requires a CUDA storage backend");
  if (!operation.has_target_count() || operation.target_count() == 0 ||
      operation.target_count() > kMaximumCudaTargets)
    return Fail(request, Failure::INVALID_REQUEST,
                "restore admission requires between 1 and 64 CUDA targets");

  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() == Transaction::State::RESTORE_ADMITTED) {
    if (transaction->restore_backend() == operation.storage_backend() &&
        transaction->restore_target_count() == operation.target_count() &&
        transaction->has_restore_admission()) {
      auto response = Reply(request);
      response.mutable_restore_admission_granted();
      return response;
    }
    return Fail(request, Failure::TRANSACTION_CONFLICT, "restore admission backend conflicts");
  }
  if (transaction->state() != Transaction::State::STAGED ||
      std::get_if<RestoreTransactionDescriptor>(&transaction->descriptor()) == nullptr)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "restore admission requires a staged restore transaction");
  const auto* descriptor =
      std::get_if<RestoreTransactionDescriptor>(&transaction->descriptor());
  if (!descriptor->supports_backend(operation.storage_backend()))
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "restore backend does not match the staged restore mode");

  size_t expected_dispatch_group_count = operation.target_count();
  if (operation.storage_backend() ==
      v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE) {
    std::error_code job_file_error;
    const auto job_file_status = fs::symlink_status(
        descriptor->staging_directory() / kCudaJobFileName, job_file_error);
    if (job_file_error &&
        job_file_error != std::errc::no_such_file_or_directory)
      return Fail(request, Failure::STORAGE_ERROR,
                  "inspect staged CUDA launch-job file: " +
                      job_file_error.message());
    if (fs::exists(job_file_status)) {
      if (!fs::is_regular_file(job_file_status))
        return Fail(request, Failure::STORAGE_ERROR,
                    "staged CUDA launch-job file is not regular");
      // One immutable artifact job file becomes one restore-time inode shared
      // by all CUDA participants in this container, so one CustomStorage
      // worker owns the complete launch-job dispatch group.
      expected_dispatch_group_count = 1;
    }
  }

  RestoreAdmissionResult result;
  try {
    result = cuda_engine_->BeginRestore(operation.storage_backend(),
                                        operation.target_count(),
                                        expected_dispatch_group_count);
  }
  catch (const std::invalid_argument& error) {
    return Fail(request, Failure::INVALID_REQUEST, error.what());
  }
  catch (const std::exception& error) {
    result.operation.target_may_be_mutated = false;
    result.operation.fatal = true;
    result.operation.error = std::string("unexpected CUDA restore admission failure: ") + error.what();
  }
  catch (...) {
    result.operation.target_may_be_mutated = false;
    result.operation.fatal = true;
    result.operation.error = "unexpected CUDA restore admission failure";
  }
  if (result.operation.fatal) {
    cuda_engine_->FailStop();
    shutdown_requested_.store(true, std::memory_order_release);
  }
  if (result.admission == nullptr)
    return FailCuda(request, result.operation);

  transaction->set_restore_admission(
      std::move(result.admission), operation.storage_backend(),
      operation.target_count());
  transaction->set_state(Transaction::State::RESTORE_ADMITTED);
  auto response = Reply(request);
  response.mutable_restore_admission_granted();
  return response;
}

Response
Broker::ActivateRestore(const Request& request)
{
  const auto& operation = request.activate_restore();
  if (!operation.has_identity())
    return Fail(request, Failure::INVALID_REQUEST,
                "restore activation identity is required");
  const auto& identity = operation.identity();
  try {
    ValidateRestoreIdentity(identity);
  }
  catch (const std::invalid_argument& error) {
    return Fail(request, Failure::INVALID_REQUEST, error.what());
  }

  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND,
                "transaction not found");
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::RESTORE_ADMITTED ||
      !transaction->has_restore_admission())
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "restore activation requires prior restore admission");
  auto* descriptor =
      std::get_if<RestoreTransactionDescriptor>(&transaction->descriptor());
  if (descriptor == nullptr || descriptor->staging_request_id().empty())
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "restore activation requires a staged restore transaction");
  if (!transaction->restore_backend().has_value())
    return Fail(request, Failure::INTERNAL_ERROR,
                "restore activation has no admitted backend");

  StageReadyBackend backend;
  switch (*transaction->restore_backend()) {
    case v1::CUDA_STORAGE_BACKEND_REGULAR:
      backend = StageReadyBackend::REGULAR;
      break;
    case v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE:
      backend = StageReadyBackend::POSIX_CUSTOM_STORAGE;
      break;
    default:
      return Fail(request, Failure::INTERNAL_ERROR,
                  "restore activation has an unsupported admitted backend");
  }
  const auto& staged_identity = descriptor->restore_identity();
  if (!staged_identity.has_value() ||
      !SameRestoreIdentity(*staged_identity, identity))
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "restore activation identity does not match staging identity");
  try {
    const auto handle = stage_ready_gate_.PublishReady({
        .node_name = stage_ready_gate_.node_name(),
        .pod_uid = identity.pod_uid(),
        .destination_container = identity.destination_container(),
        .content_uid = identity.content_uid(),
        .source_container = identity.source_container(),
        .container_id = identity.container_id(),
        .transaction_id = request.transaction_id(),
        .stage_request_id = descriptor->staging_request_id(),
        .backend = backend,
    });
    descriptor->set_stage_ready_handle(handle);
  }
  catch (const StageReadyDurabilityUncertain& error) {
    descriptor->set_stage_ready_handle(error.handle());
    throw;
  }
  auto response = Reply(request);
  response.mutable_restore_activation_granted();
  return response;
}

Response
Broker::ExecuteCudaRestore(const Request& request)
{
  auto transaction = FindTransaction(request.transaction_id());
  if (!transaction)
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "transaction not found");
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::RESTORE_ADMITTED || !transaction->has_restore_admission())
    return Fail(request, Failure::TRANSACTION_CONFLICT, "CUDA restore requires prior restore admission");
  const auto* descriptor = std::get_if<RestoreTransactionDescriptor>(&transaction->descriptor());
  if (descriptor == nullptr)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "CUDA restore does not match transaction kind");
  if (!request.cuda_restore().has_storage_backend() ||
      transaction->restore_backend() != request.cuda_restore().storage_backend())
    return Fail(request, Failure::TRANSACTION_CONFLICT, "CUDA restore backend does not match admission");
  if (transaction->restore_target_count() !=
      static_cast<size_t>(request.cuda_restore().targets_size()))
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "CUDA restore target count does not match admission");

  if (descriptor->direct_carriers()) {
    if (request.cuda_restore().storage_backend() !=
        v1::CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE)
      return Fail(request, Failure::TRANSACTION_CONFLICT,
                  "direct carriers require POSIX CustomStorage restore");
    if (!descriptor->has_exact_cuda_namespace_pids(request.cuda_restore()))
      return Fail(request, Failure::TRANSACTION_CONFLICT,
                  "CUDA restore targets must exactly match the direct carrier set");
  }

  auto admission = transaction->take_restore_admission();
  transaction->set_state(Transaction::State::CUDA_STARTED);
  CudaOperationResult result;
  try {
    result = cuda_engine_->Restore(
        request.cuda_restore(), descriptor->staging_directory(), *admission,
        descriptor->direct_carriers() ? &descriptor->direct_processes()
                                      : nullptr);
  }
  catch (const std::invalid_argument&) {
    throw;
  }
  catch (const std::exception& error) {
    result.target_may_be_mutated = true;
    result.fatal = true;
    result.error = std::string("unexpected CUDA engine failure: ") + error.what();
  }
  catch (...) {
    result.target_may_be_mutated = true;
    result.fatal = true;
    result.error = "unexpected CUDA engine failure";
  }
  if (result.fatal) {
    cuda_engine_->FailStop();
    shutdown_requested_.store(true, std::memory_order_release);
  }
  if (!result.succeeded)
    return FailCuda(request, result);
  transaction->set_state(Transaction::State::CUDA_COMPLETE);
  auto response = Reply(request);
  response.mutable_cuda_operation_complete()->set_target_count(result.target_count);
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
Broker::ReferenceRegularRestore(const Request& request)
{
  const auto stage_started = std::chrono::steady_clock::now();
  const auto& operation = request.reference_regular_restore();
  if (!operation.has_source() ||
      operation.source().kind_case() == StorageBackend::KIND_NOT_SET)
    return Fail(request, Failure::INVALID_REQUEST,
                "regular restore reference source is required");
  try {
    if (!operation.has_pod_uid() || !operation.has_destination_container() ||
        !operation.has_content_uid() || !operation.has_source_container() ||
        !operation.has_container_id())
      throw std::invalid_argument(
          "regular restore identity fields are required");
    ValidateRestoreIdentityField(operation.pod_uid(), "pod UID");
    ValidateRestoreIdentityField(operation.destination_container(),
                                 "destination container");
    ValidateRestoreIdentityField(operation.content_uid(), "content UID");
    ValidateRestoreIdentityField(operation.source_container(),
                                 "source container");
    ValidateRestoreIdentityField(operation.container_id(), "container ID");
  }
  catch (const std::invalid_argument& error) {
    return Fail(request, Failure::INVALID_REQUEST, error.what());
  }
  const auto& engine = Engine(operation.io_engine());
  const auto* posix = dynamic_cast<const PosixCopyEngine*>(&engine);
  if (posix == nullptr)
    return Fail(request, Failure::INVALID_REQUEST,
                "regular restore reference requires the POSIX copy engine");

  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW)
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "restore transaction conflicts");
  transaction->set_state(Transaction::State::PREPARING);
  Path reference_directory;
  try {
    reference_directory = posix->ReferenceRegularRestore(
        operation.source(), request.transaction_id());
    RestoreTransactionDescriptor descriptor(
        reference_directory,
        RestoreTransactionDescriptor::Kind::REGULAR_REFERENCE);
    descriptor.set_staging_request_id(request.request_id());
    RestoreIdentity identity;
    identity.set_pod_uid(operation.pod_uid());
    identity.set_destination_container(operation.destination_container());
    identity.set_content_uid(operation.content_uid());
    identity.set_source_container(operation.source_container());
    identity.set_container_id(operation.container_id());
    descriptor.set_restore_identity(std::move(identity));
    transaction->set_descriptor(std::move(descriptor));
    transaction->set_state(Transaction::State::STAGED);
  }
  catch (const std::invalid_argument& error) {
    transaction->set_state(Transaction::State::ABORTED);
    return Fail(request, Failure::INVALID_REQUEST, error.what());
  }
  catch (const std::exception& error) {
    transaction->set_state(Transaction::State::ABORTED);
    return Fail(request, Failure::STORAGE_ERROR, error.what());
  }
  std::fprintf(
      stdout,
      "{\"event\":\"pagebroker_regular_restore_reference\","
      "\"schema_version\":1,\"request_id\":\"%s\","
      "\"transaction_id\":\"%s\","
      "\"stage_duration_seconds\":%.9f,\"copied_bytes\":0}\n",
      JsonEscape(request.request_id()).c_str(),
      JsonEscape(request.transaction_id()).c_str(),
      std::chrono::duration<double>(std::chrono::steady_clock::now() -
                                    stage_started).count());
  std::fflush(stdout);
  auto response = Reply(request);
  response.mutable_staged_restore_directory()->set_image_directory(
      reference_directory.string());
  return response;
}

Response
Broker::DirectRestore(const Request& request)
{
  const auto stage_started = std::chrono::steady_clock::now();
  const auto& operation = request.direct_restore();
  if (!operation.has_source() ||
      operation.source().kind_case() == StorageBackend::KIND_NOT_SET)
    return Fail(request, Failure::INVALID_REQUEST,
                "direct restore source is required");
  if (!operation.has_identity())
    return Fail(request, Failure::INVALID_REQUEST,
                "direct restore identity is required");
  try {
    ValidateRestoreIdentity(operation.identity());
  }
  catch (const std::invalid_argument& error) {
    return Fail(request, Failure::INVALID_REQUEST, error.what());
  }
  const auto& engine = Engine(operation.io_engine());
  const auto* posix = dynamic_cast<const PosixCopyEngine*>(&engine);
  if (posix == nullptr)
    return Fail(request, Failure::INVALID_REQUEST,
                "direct restore requires the POSIX copy engine");
  std::vector<uint32_t> pids(
      operation.cuda_namespace_pids().begin(),
      operation.cuda_namespace_pids().end());
  size_t descriptor_count = 0;
  {
    // Descriptor counting opens each process directory and manifest briefly.
    // Serialize those transient opens so concurrent preflights cannot consume
    // the RLIMIT headroom reserved for the daemon and active requests.
    std::lock_guard descriptor_lock(direct_restore_descriptor_mutex_);
    descriptor_count =
        posix->DirectRestoreDescriptorCount(operation.source(), pids);
  }

  const Path restore_root = staging_root_ / "restore";
  const Path staging_directory =
      TransactionDirectory(restore_root, request.transaction_id());
  auto transaction = CreateOrGetTransaction(request.transaction_id());
  std::lock_guard lock(transaction->mutex());
  if (transaction->state() != Transaction::State::NEW ||
      fs::exists(staging_directory))
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "restore transaction conflicts");
  if (!ReserveDirectRestoreDescriptors(descriptor_count))
    return FailCuda(request, {
      .target_may_be_mutated = false,
      .failure_code = Failure::BUSY,
      .error = "PageBroker direct restore descriptor capacity is busy",
    });
  transaction->set_direct_restore_descriptor_reservation(descriptor_count);
  uintmax_t bytes = 0;
  uintmax_t retained_bytes = 0;
  bool copy_pending = false;
  try {
    auto plan = posix->PrepareDirectRestore(
        operation.source(), pids, descriptor_count);
    bytes = plan.copied_bytes;
    retained_bytes = plan.retained_bytes;
    if (!ReserveStaging(bytes, true)) {
      plan = DirectRestorePlan{};
      ReleaseDirectRestoreDescriptors(*transaction);
      return Fail(request, Failure::INSUFFICIENT_STORAGE,
                  "insufficient staging capacity");
    }
    transaction->set_staging_reservation_bytes(bytes);
    copy_pending = true;
    transaction->set_state(Transaction::State::PREPARING);
    auto staged = posix->StageDirectRestore(std::move(plan), staging_directory);
    if (staged.copied_bytes != bytes ||
        staged.retained_bytes != retained_bytes) {
      const bool admitted_size_exceeded = staged.copied_bytes > bytes;
      FinishStagingCopy(bytes);
      copy_pending = false;
      std::error_code cleanup_error;
      if (!CleanupStaging(*transaction, staging_directory, cleanup_error))
        return Fail(request, Failure::STORAGE_ERROR,
                    "cleanup size-mismatched direct restore staging: " +
                        cleanup_error.message());
      staged = DirectRestoreStage{};
      transaction->clear_descriptor();
      ReleaseDirectRestoreDescriptors(*transaction);
      transaction->set_state(Transaction::State::ABORTED);
      if (admitted_size_exceeded)
        return Fail(request, Failure::INSUFFICIENT_STORAGE,
                    "direct restore staging exceeded its admitted size");
      return Fail(request, Failure::STORAGE_ERROR,
                  "direct restore metadata changed while it was copied");
    }
    FinishStagingCopy(bytes);
    copy_pending = false;
    if (staged.retained_bytes >
        std::numeric_limits<uintmax_t>::max() - staged.copied_bytes)
      throw std::overflow_error("direct restore source size overflow");
    const uintmax_t total = staged.copied_bytes + staged.retained_bytes;
    const double amplification = total == 0
                                     ? 0.0
                                     : static_cast<double>(staged.copied_bytes) /
                                           static_cast<double>(total);
    std::fprintf(
        stdout,
        "{\"event\":\"pagebroker_direct_restore_stage\","
        "\"schema_version\":1,\"request_id\":\"%s\","
        "\"transaction_id\":\"%s\",\"target_count\":%zu,"
        "\"stage_duration_seconds\":%.9f,\"copied_bytes\":%ju,"
        "\"retained_carrier_bytes\":%ju,\"source_bytes\":%ju,"
        "\"copy_amplification\":%.9f}\n",
        JsonEscape(request.request_id()).c_str(),
        JsonEscape(request.transaction_id()).c_str(), staged.processes.size(),
        std::chrono::duration<double>(std::chrono::steady_clock::now() -
                                      stage_started).count(),
        static_cast<uintmax_t>(staged.copied_bytes),
        static_cast<uintmax_t>(staged.retained_bytes), total, amplification);
    std::fflush(stdout);
    RestoreTransactionDescriptor descriptor(
        staging_directory, std::move(staged.source_directory),
        std::move(staged.processes), staged.retained_bytes);
    descriptor.set_staging_request_id(request.request_id());
    descriptor.set_restore_identity(operation.identity());
    transaction->set_descriptor(std::move(descriptor));
    transaction->set_state(Transaction::State::STAGED);
  }
  catch (const StagingCapacityExceeded& error) {
    if (copy_pending)
      FinishStagingCopy(bytes);
    return AbortStaging(request, *transaction, staging_directory, error,
                        Failure::INSUFFICIENT_STORAGE);
  }
  catch (const std::exception& error) {
    if (copy_pending)
      FinishStagingCopy(bytes);
    return AbortStaging(request, *transaction, staging_directory, error);
  }
  auto response = Reply(request);
  response.mutable_staged_restore_directory()->set_image_directory(
      staging_directory.string());
  return response;
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
  if (!ReserveStaging(bytes, true)) {
    return Fail(request, Failure::INSUFFICIENT_STORAGE, "insufficient staging capacity");
  }
  transaction->set_staging_reservation_bytes(bytes);
  bool copy_pending = true;
  try {
    transaction->set_state(Transaction::State::PREPARING);
    const uintmax_t copied_bytes =
        engine.StageRestore(source, staging_directory, bytes);
    if (copied_bytes != bytes) {
      FinishStagingCopy(bytes);
      copy_pending = false;
      std::error_code cleanup_error;
      if (!CleanupStaging(*transaction, staging_directory, cleanup_error))
        return Fail(request, Failure::STORAGE_ERROR, "cleanup size-mismatched restore staging: " + cleanup_error.message());
      transaction->clear_descriptor();
      transaction->set_state(Transaction::State::ABORTED);
      if (copied_bytes > bytes)
        return Fail(request, Failure::INSUFFICIENT_STORAGE, "staged restore exceeded its admitted size");
      return Fail(request, Failure::STORAGE_ERROR, "staged restore changed size while it was copied");
    }
    FinishStagingCopy(bytes);
    copy_pending = false;
    RestoreTransactionDescriptor descriptor(staging_directory);
    descriptor.set_staging_request_id(request.request_id());
    transaction->set_descriptor(std::move(descriptor));
    transaction->set_state(Transaction::State::STAGED);
  }
  catch (const StagingCapacityExceeded& error) {
    if (copy_pending)
      FinishStagingCopy(bytes);
    return AbortStaging(request, *transaction, staging_directory, error,
                        Failure::INSUFFICIENT_STORAGE);
  }
  catch (const std::exception& error) {
    if (copy_pending)
      FinishStagingCopy(bytes);
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
  if (!ReserveStaging(max_staging_bytes_, false)) {
    return Fail(request, Failure::INSUFFICIENT_STORAGE, "insufficient staging capacity");
  }
  transaction->set_staging_reservation_bytes(max_staging_bytes_);
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
  if (transaction->state() == Transaction::State::PREPARING)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "transaction is preparing");
  if (transaction->state() == Transaction::State::CHECKPOINT_ADMITTED ||
      transaction->state() == Transaction::State::RESTORE_ADMITTED ||
      transaction->state() == Transaction::State::CUDA_STARTED)
    return Fail(request, Failure::TRANSACTION_CONFLICT,
                "CUDA transaction has not completed its operation");
  if (transaction->state() == Transaction::State::COMMITTED)
    return CommitSucceeded(request);

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
  if (descriptor.stage_ready_handle().has_value())
    stage_ready_gate_.Transition(*descriptor.stage_ready_handle(),
                                 StageReadyState::COMMITTED);
  std::error_code error;
  if (!CleanupStaging(transaction, descriptor.staging_directory(), error))
    return Fail(request, Failure::STORAGE_ERROR, "cleanup restore staging: " + error.message());
  transaction.clear_descriptor();
  ReleaseDirectRestoreDescriptors(transaction);
  transaction.set_state(Transaction::State::COMMITTED);
  return CommitSucceeded(request);
}

Response
Broker::PublishCheckpoint(
    const Request& request, Transaction& transaction, const CheckpointTransactionDescriptor& descriptor)
{
  const Path staging_directory = descriptor.staging_directory();
  if (transaction.state() != Transaction::State::PUBLISHED_CLEANUP_PENDING && !fs::is_directory(staging_directory))
    return Fail(request, Failure::TRANSACTION_NOT_FOUND, "checkpoint staging directory not found");
  if (transaction.state() != Transaction::State::PUBLISHED_CLEANUP_PENDING) {
    const auto& engine = Engine(descriptor.engine_type());
    if (engine.CheckpointDestinationConflicts(descriptor.destination_storage()))
      return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint destination conflicts");
    RejectSymlinks(staging_directory);
    try {
      engine.PublishCheckpoint(staging_directory, descriptor.destination_storage());
      transaction.set_state(Transaction::State::PUBLISHED_CLEANUP_PENDING);
    }
    catch (const std::exception& error) {
      return Fail(request, Failure::STORAGE_ERROR, error.what());
    }
  }
  std::error_code cleanup_error;
  if (!CleanupStaging(transaction, staging_directory, cleanup_error))
    return Fail(request, Failure::STORAGE_ERROR, "cleanup checkpoint staging: " + cleanup_error.message());
  transaction.clear_descriptor();
  transaction.set_state(Transaction::State::COMMITTED);
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

  const bool checkpoint_published = transaction->state() == Transaction::State::PUBLISHED_CLEANUP_PENDING;

  Path restore_directory = TransactionDirectory(
      staging_root_ / "restore", request.transaction_id());
  if (const auto* descriptor =
          std::get_if<RestoreTransactionDescriptor>(
          &transaction->descriptor()))
    restore_directory = descriptor->staging_directory();
  if (const auto* descriptor =
          std::get_if<RestoreTransactionDescriptor>(
              &transaction->descriptor());
      descriptor != nullptr && descriptor->stage_ready_handle().has_value())
    stage_ready_gate_.Transition(*descriptor->stage_ready_handle(),
                                 StageReadyState::ABORTED);
  const Path checkpoint_root = staging_root_ / "checkpoint";
  std::error_code restore_error;
  std::error_code checkpoint_error;
  fs::remove_all(restore_directory, restore_error);
  fs::remove_all(TransactionDirectory(checkpoint_root, request.transaction_id()), checkpoint_error);
  if (restore_error || checkpoint_error) {
    const std::string message = restore_error ? restore_error.message() : checkpoint_error.message();
    return Fail(request, Failure::STORAGE_ERROR, "cleanup staging: " + message);
  }
  ReleaseStaging(*transaction);
  transaction->clear_restore_admission();
  transaction->clear_checkpoint_admission();
  transaction->clear_descriptor();
  ReleaseDirectRestoreDescriptors(*transaction);
  transaction->set_state(checkpoint_published ? Transaction::State::COMMITTED : Transaction::State::ABORTED);
  if (checkpoint_published)
    return Fail(request, Failure::TRANSACTION_CONFLICT, "checkpoint was already published");
  return AbortSucceeded(request);
}

}  // namespace snapshot::pagebroker
