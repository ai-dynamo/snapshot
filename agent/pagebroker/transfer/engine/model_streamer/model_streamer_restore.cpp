// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_restore.hpp"

#include <fcntl.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <chrono>
#include <exception>
#include <filesystem>
#include <future>
#include <limits>
#include <stdexcept>
#include <string>
#include <system_error>
#include <unordered_map>
#include <utility>
#include <vector>

#include "file_descriptor.hpp"
#include <streamer/streamer.h>

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
// Credential keys documented by Model Streamer's public C API.
constexpr char kRegionKey[] = "region";
constexpr char kEndpointKey[] = "endpoint";
constexpr char kAccessKeyIdKey[] = "access_key_id";
constexpr char kSecretAccessKeyKey[] = "secret_access_key";
constexpr char kSessionTokenKey[] = "session_token";

// Bound each batch and the bytes admitted concurrently. This 10 GB target
// avoids submitting a whole large checkpoint at once; a larger file runs alone.
constexpr uintmax_t kSubmissionByteBudget = 10'000'000'000;
constexpr unsigned kResponsePollTimeoutMs = 25;
constexpr auto kCancellationPollInterval = std::chrono::milliseconds(25);
// Bound outstanding submissions while reusing the native session across restores.
// Failed submissions retain their mappings until the session drains.
constexpr std::size_t kActiveSubmissionLimit = 8;

bool
IsRecoverableRangeStatus(int status)
{
  switch (status) {
    case RUNAI_FILE_STREAMER_RESPONSE_SUCCESS:
    case RUNAI_FILE_STREAMER_RESPONSE_FILE_ACCESS_ERROR:
    case RUNAI_FILE_STREAMER_RESPONSE_EOF_ERROR:
    case RUNAI_FILE_STREAMER_RESPONSE_FILE_TRUNCATED_ERROR:
      return true;
    default:
      return false;
  }
}


bool
HasCompleteCredentials(const ModelStreamerSessionOptions& options)
{
  if (options.access_key_id.empty() != options.secret_access_key.empty())
    return false;
  return options.session_token.empty() || !options.access_key_id.empty();
}

std::system_error
SystemError(const char* operation)
{
  return std::system_error(errno, std::generic_category(), operation);
}

std::string
StreamerError(int response)
{
  const char* description = runai_file_streamer_response_str(response);
  return description == nullptr ? "unknown Model Streamer error" : description;
}

void
ApplyPermissions(const Path& path, fs::perms permissions, Cancellation cancellation)
{
  cancellation.ThrowIfCancelled();
  if (permissions != fs::perms::unknown)
    fs::permissions(path, permissions, fs::perm_options::replace);
}

size_t
CheckedRestoreFileSize(uintmax_t bytes)
{
  if (bytes == 0 || bytes > std::numeric_limits<size_t>::max() ||
      bytes > static_cast<uintmax_t>(std::numeric_limits<off_t>::max()))
    throw std::runtime_error("invalid restore file size");
  return static_cast<size_t>(bytes);
}

void
CreateDirectoryTree(const CheckpointInventory& inventory, const Path& destination, Cancellation cancellation)
{
  fs::create_directory(destination);
  cancellation.ThrowIfCancelled();
  for (const auto& directory : inventory.directories) {
    fs::create_directory(destination / directory.relative_path);
    cancellation.ThrowIfCancelled();
  }
}

void
CreateEmptyFile(const CheckpointFile& file, const Path& destination, Cancellation cancellation)
{
  FileDescriptor descriptor(open(destination.c_str(), O_CREAT | O_EXCL | O_WRONLY | O_CLOEXEC, 0600));
  if (descriptor.get() < 0)
    throw SystemError("create empty restore staging file");
  // Empty files need no native submission; the backend checks remote existence.
  ApplyPermissions(destination, file.permissions, cancellation);
}

void
ApplyTreePermissions(const CheckpointInventory& inventory, const Path& destination, Cancellation cancellation)
{
  for (auto directory = inventory.directories.rbegin(); directory != inventory.directories.rend(); ++directory) {
    ApplyPermissions(destination / directory->relative_path, directory->permissions, cancellation);
  }
  ApplyPermissions(destination, inventory.root_permissions, cancellation);
  cancellation.ThrowIfCancelled();
}
}  // namespace

ModelStreamerRestore::StreamerEntry::~StreamerEntry() noexcept
{
  ReleaseMappings();
}

bool
ModelStreamerRestore::StreamerEntry::TryAddFile(const CheckpointFile& file, const Path& destination)
{
  if (!request.paths.empty() &&
      (bytes >= kSubmissionByteBudget || file.size_bytes > kSubmissionByteBudget - bytes))
    return false;
  if (request.paths.size() == std::numeric_limits<unsigned>::max())
    throw std::runtime_error("too many files in one Model Streamer submission");

  const size_t file_bytes = CheckedRestoreFileSize(file.size_bytes);
  request.paths.push_back(file.source_locator.c_str());
  request.range_counts.push_back(1);
  request.offsets.push_back(0);
  request.sizes.push_back(file_bytes);
  request.destinations.push_back(MAP_FAILED);
  completed.push_back(false);

  FileDescriptor descriptor(open(destination.c_str(), O_CREAT | O_EXCL | O_RDWR | O_CLOEXEC, 0600));
  if (descriptor.get() < 0)
    throw SystemError("create restore staging file");
  // Reserve backing storage before native writes; sparse mappings can SIGBUS on ENOSPC.
  const int allocation_error = posix_fallocate(descriptor.get(), 0, static_cast<off_t>(file_bytes));
  if (allocation_error != 0)
    throw std::system_error(allocation_error, std::generic_category(), "allocate restore staging file");
  cancellation.ThrowIfCancelled();
  void* address = mmap(nullptr, file_bytes, PROT_READ | PROT_WRITE, MAP_SHARED, descriptor.get(), 0);
  if (address == MAP_FAILED)
    throw SystemError("map restore staging file");
  request.destinations.back() = address;
  // Native reads write through this mapping, so final file permissions can be
  // applied before submission. Record the mapping first for failure cleanup.
  ApplyPermissions(destination, file.permissions, cancellation);
  bytes += file_bytes;
  return true;
}

void
ModelStreamerRestore::StreamerEntry::ReleaseMappings() noexcept
{
  for (size_t i = 0; i < request.destinations.size(); ++i) {
    if (request.destinations[i] != MAP_FAILED) {
      munmap(request.destinations[i], request.sizes[i]);
      request.destinations[i] = MAP_FAILED;
    }
  }
}

ModelStreamerRestore::ModelStreamerRestore(std::chrono::milliseconds submission_timeout)
    : ModelStreamerRestore(ModelStreamerSessionOptions{}, submission_timeout)
{
}

ModelStreamerRestore::ModelStreamerRestore(
    ModelStreamerSessionOptions options,
    std::chrono::milliseconds submission_timeout)
    : submission_timeout_(submission_timeout), options_(std::move(options)),
      stopped_error_(std::make_exception_ptr(std::runtime_error("Model Streamer restore stopped")))
{
  if (submission_timeout_ <= std::chrono::milliseconds::zero())
    throw std::invalid_argument("Model Streamer submission timeout must be positive");
  if (!HasCompleteCredentials(options_))
    throw std::invalid_argument("Model Streamer explicit credentials require both access and secret keys");
  for (const auto* value : {&options_.region, &options_.endpoint, &options_.access_key_id,
                            &options_.secret_access_key, &options_.session_token}) {
    if (value->find('\0') != std::string::npos)
      throw std::invalid_argument("Model Streamer session options must not contain NUL bytes");
  }
}

ModelStreamerRestore::~ModelStreamerRestore() noexcept
{
  RequestStop();
  if (worker_.joinable())
    worker_.join();
}

bool
ModelStreamerRestore::Failed() const noexcept
{
  return failed_.load(std::memory_order_acquire);
}

void
ModelStreamerRestore::RequestStop() noexcept
{
  CloseAdmission(stopped_error_);
}

std::exception_ptr
ModelStreamerRestore::CloseAdmission(std::exception_ptr error) noexcept
{
  {
    std::lock_guard lock(pending_mutex_);
    if (!terminal_error_)
      terminal_error_ = std::move(error);
    error = terminal_error_;
  }
  work_ready_.notify_one();
  return error;
}

std::exception_ptr
ModelStreamerRestore::WaitForWork()
{
  std::unique_lock lock(pending_mutex_);
  work_ready_.wait(lock, [this] { return terminal_error_ || !pending_.empty() || !active_.empty(); });
  return terminal_error_;
}

void
ModelStreamerRestore::Run() noexcept
{
  std::exception_ptr error;
  bool failed = false;
  try {
    while (true) {
      error = WaitForWork();
      if (error)
        break;
      // The native ABI permits concurrent submissions, but request and response
      // calls must be serialized. Only this worker calls the native API.
      ProcessWork();
    }
  }
  catch (...) {
    failed = true;
    error = CloseAdmission(std::current_exception());
  }

  // Close admission before teardown, but allow engine replacement only after
  // native access ends. Waiters may release mappings as soon as we complete them.
  StopStreamer();
  if (failed)
    failed_.store(true, std::memory_order_release);
  FailAll(std::move(error));
}

void
ModelStreamerRestore::Start()
{
  const int response = runai_file_streamer_start(&streamer_handle_);
  if (response != 0)
    throw std::runtime_error("start Model Streamer: " + StreamerError(response));
  if (streamer_handle_ == nullptr)
    throw std::runtime_error("Model Streamer started without returning a handle");

  // The worker owns teardown, including partial startup.
  std::vector<const char*> keys;
  std::vector<const char*> values;
  const auto append = [&](const char* key, const std::string& value) {
    if (!value.empty()) {
      keys.push_back(key);
      values.push_back(value.c_str());
    }
  };
  append(kRegionKey, options_.region);
  append(kEndpointKey, options_.endpoint);
  append(kAccessKeyIdKey, options_.access_key_id);
  append(kSecretAccessKeyKey, options_.secret_access_key);
  append(kSessionTokenKey, options_.session_token);
  if (!keys.empty()) {
    const int status = runai_file_streamer_set_credentials(
        streamer_handle_, keys.data(), values.data(), static_cast<unsigned>(keys.size()));
    if (status != 0)
      throw std::runtime_error("configure Model Streamer session: " + StreamerError(status));
  }
}

void
ModelStreamerRestore::QueueSubmission(const std::shared_ptr<StreamerEntry>& entry)
{
  std::exception_ptr error;
  {
    std::lock_guard lock(pending_mutex_);
    error = terminal_error_;
    if (!error)
      pending_.push_back(entry);
  }
  if (error)
    FailEntry(*entry, std::move(error));
  else
    work_ready_.notify_one();
}

std::shared_ptr<ModelStreamerRestore::StreamerEntry>
ModelStreamerRestore::SelectPendingSubmission(bool& stopped)
{
  std::lock_guard lock(pending_mutex_);
  stopped = terminal_error_ != nullptr;
  if (stopped || pending_.empty() || !CanAdmit(*pending_.front()))
    return {};
  return pending_.front();
}

void
ModelStreamerRestore::RemoveHandledSubmission()
{
  std::lock_guard lock(pending_mutex_);
  pending_.pop_front();
}

std::shared_ptr<ModelStreamerRestore::StreamerEntry>
ModelStreamerRestore::TakeInterruptedSubmission(std::exception_ptr& error)
{
  std::lock_guard lock(pending_mutex_);
  for (auto entry = pending_.begin(); entry != pending_.end(); ++entry) {
    if (const auto reason = (*entry)->cancellation.Reason()) {
      error = std::make_exception_ptr(CancellationError(*reason));
      auto interrupted = std::move(*entry);
      pending_.erase(entry);
      return interrupted;
    }
  }
  error = nullptr;
  return {};
}

std::shared_ptr<ModelStreamerRestore::StreamerEntry>
ModelStreamerRestore::TakePendingSubmission()
{
  std::lock_guard lock(pending_mutex_);
  if (pending_.empty())
    return {};
  auto entry = std::move(pending_.front());
  pending_.pop_front();
  return entry;
}

bool
ModelStreamerRestore::TryClaimSubmission(StreamerEntry& entry, bool& stopped)
{
  std::lock_guard lock(pending_mutex_);
  stopped = terminal_error_ != nullptr;
  if (stopped)
    return false;
  auto expected = Admission::Pending;
  return entry.admission.compare_exchange_strong(expected, Admission::Claimed);
}

bool
ModelStreamerRestore::CanAdmit(const StreamerEntry& entry) const
{
  // Stop admission on the first storage error so existing work can drain and
  // failed mappings can be released after native teardown.
  if (active_.size() >= kActiveSubmissionLimit ||
      std::any_of(active_.begin(), active_.end(), [](const auto& item) { return !item.second->first_error.empty(); }))
    return false;
  // An oversized file may run by itself.
  if (active_.empty())
    return true;
  if (active_bytes_ >= kSubmissionByteBudget)
    return false;
  return entry.bytes <= kSubmissionByteBudget - active_bytes_;
}

void
ModelStreamerRestore::SubmitPending()
{
  DiscardInterruptedPending();
  while (true) {
    bool stopped = false;
    const auto entry = SelectPendingSubmission(stopped);
    if (stopped)
      return;
    if (!entry)
      break;
    if (streamer_handle_ == nullptr)
      Start();
    // Retain the queued reference until submission and registration finish.
    // On exception, Run stops native access before completing even an entry
    // that the library accepted but we could not register in active_.
    if (!AdmitAndSubmit(entry))
      return;
    RemoveHandledSubmission();
  }
}

void
ModelStreamerRestore::DiscardInterruptedPending()
{
  while (true) {
    std::exception_ptr error;
    auto entry = TakeInterruptedSubmission(error);
    if (!entry)
      return;
    FailEntry(*entry, std::move(error));
  }
}

bool
ModelStreamerRestore::AdmitAndSubmit(const std::shared_ptr<StreamerEntry>& entry)
{
  if (const auto reason = entry->cancellation.Reason()) {
    // Cancellation can race with session startup or admission. No native
    // request owns this entry yet, so it must not fail other submissions.
    FailEntry(*entry, std::make_exception_ptr(CancellationError(*reason)));
    return true;  // The entry was handled; remove it and continue with queued work.
  }
  bool stopped = false;
  if (!TryClaimSubmission(*entry, stopped)) {
    if (stopped)
      return false;
    return true;  // The caller cancelled and may already have released its buffers.
  }

  const auto submission_id = SubmitToStreamer(*entry);
  if (submission_id == 0)
    return true;

  entry->deadline = std::min(entry->cancellation.Deadline(), std::chrono::steady_clock::now() + submission_timeout_);
  const auto [active, inserted] = active_.try_emplace(submission_id);
  if (!inserted)
    throw std::runtime_error("Model Streamer returned a duplicate submission ID");
  active_bytes_ += entry->bytes;
  ++unfinished_;
  active->second = entry;
  return true;
}

std::uint64_t
ModelStreamerRestore::SubmitToStreamer(StreamerEntry& entry)
{
  auto& request = entry.request;
  RunaiFileStreamerSubmissionId submission_id = 0;
  const int response = runai_file_streamer_request(
      streamer_handle_, &submission_id, static_cast<unsigned>(request.paths.size()), request.paths.data(),
      request.range_counts.data(), request.offsets.data(), request.sizes.data(), request.destinations.data(),
      RunaiFileStreamerDevice{RUNAI_FILE_STREAMER_DEVICE_CPU, 0});
  if (response != 0) {
    const std::runtime_error error("submit Model Streamer restore: " + StreamerError(response));
    if (response == RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR || submission_id != 0)
      throw error;
    FailEntry(entry, std::make_exception_ptr(error));
    return 0;
  }
  if (submission_id == 0)
    throw std::runtime_error("Model Streamer accepted a submission without assigning an ID");

  return submission_id;
}

bool
ModelStreamerRestore::AcceptEntry(StreamerEntry& entry, const StreamerResponse& response)
{
  if (entry.responses_received >= entry.completed.size())
    throw std::runtime_error("Model Streamer returned too many responses for a submission");

  if (response.status != 0 && entry.first_error.empty())
    entry.first_error = "Model Streamer restore range: " + StreamerError(response.status);

  if (response.file_index >= entry.completed.size())
    throw std::runtime_error("Model Streamer returned an invalid file index");
  if (response.range_index != 0 || entry.completed[response.file_index])
    throw std::runtime_error("Model Streamer returned an invalid or duplicate range response");
  entry.completed[response.file_index] = true;

  ++entry.responses_received;
  const int expected_done = entry.responses_received == entry.completed.size() ? 1 : 0;
  if (response.submission_done != expected_done)
    throw std::runtime_error("Model Streamer reported inconsistent submission completion");
  return expected_done != 0;
}

void
ModelStreamerRestore::FailEntry(StreamerEntry& entry, std::exception_ptr error) noexcept
{
  // A shared session can fail because a different request was cancelled.
  // Only this entry's cancellation can establish its own cancellation reason.
  try {
    if (const auto reason = entry.cancellation.Reason()) {
      error = std::make_exception_ptr(CancellationError(*reason));
    }
    else {
      try {
        std::rethrow_exception(error);
      }
      catch (const CancellationError&) {
        error = std::make_exception_ptr(
            std::runtime_error("Model Streamer session interrupted by another submission's cancellation"));
      }
    }
  }
  catch (...) {
    error = std::current_exception();
  }
  try {
    entry.completion.set_exception(std::move(error));
  }
  catch (...) {
  }
}

void
ModelStreamerRestore::ProcessWork()
{
  SubmitPending();
  if (active_.empty())
    return;

  StreamerResponse response;
  std::shared_ptr<StreamerEntry> completed;
  response.status = runai_file_streamer_response(
      streamer_handle_, &response.submission_id, &response.file_index, &response.range_index, &response.submission_done,
      kResponsePollTimeoutMs);
  if (response.status != RUNAI_FILE_STREAMER_RESPONSE_TIMED_OUT) {
    if (!IsRecoverableRangeStatus(response.status))
      throw std::runtime_error("Model Streamer session failed: " + StreamerError(response.status));
    const auto entry = active_.find(response.submission_id);
    if (entry == active_.end())
      throw std::runtime_error("Model Streamer returned a response for an unknown submission");
    if (AcceptEntry(*entry->second, response)) {
      --unfinished_;
      if (entry->second->first_error.empty())
        completed = entry->second;
    }
  }

  const auto now = std::chrono::steady_clock::now();
  for (const auto& [submission_id, entry] : active_) {
    // The native API cannot cancel one submission. Failing the worker stops
    // the shared streamer before waking every Stage call in Run.
    entry->cancellation.ThrowIfCancelled();
    if (now >= entry->deadline)
      throw std::runtime_error("Model Streamer submission " + std::to_string(submission_id) + " timed out");
  }

  if (completed) {
    // Every CPU/S3 range succeeded: native access to this entry has ended,
    // independently of other submissions. Remove it before waking its caller,
    // which may immediately release the mappings and the inventory's source strings.
    active_bytes_ -= completed->bytes;
    active_.erase(response.submission_id);
    completed->completion.set_value();
  }

  // Healthy idle sessions stay open; only failed entries need a native drain.
  if (unfinished_ == 0 && !active_.empty())
    FinishFailedSession();
}

void
ModelStreamerRestore::FinishFailedSession()
{
  StopStreamer();
  for (auto& [id, entry] : active_) {
    FailEntry(*entry, std::make_exception_ptr(std::runtime_error(entry->first_error)));
  }
  active_.clear();
  active_bytes_ = 0;
}

void
ModelStreamerRestore::StopStreamer() noexcept
{
  if (streamer_handle_ != nullptr) {
    runai_file_streamer_end(streamer_handle_);
    streamer_handle_ = nullptr;
  }
}

void
ModelStreamerRestore::FailAll(std::exception_ptr error) noexcept
{
  for (auto& [id, entry] : active_)
    FailEntry(*entry, error);
  active_.clear();
  while (auto entry = TakePendingSubmission())
    FailEntry(*entry, error);
  unfinished_ = 0;
  active_bytes_ = 0;
}

void
ModelStreamerRestore::SubmitAndWait(const std::shared_ptr<StreamerEntry>& entry)
{
  auto completion = entry->completion.get_future();
  QueueSubmission(entry);
  auto& admission = entry->admission;
  const auto& cancellation = entry->cancellation;
  while (completion.wait_for(std::chrono::milliseconds::zero()) != std::future_status::ready) {
    if (const auto reason = cancellation.Reason()) {
      auto expected = Admission::Pending;
      if (admission.compare_exchange_strong(expected, Admission::Cancelled))
        throw CancellationError(*reason);
      // The worker won admission. Wait for successful completion or native
      // teardown before releasing mappings, even after the caller's deadline.
      break;
    }
    if (admission.load() == Admission::Claimed)
      break;
    completion.wait_until(std::min(
        cancellation.Deadline(), Cancellation::Clock::now() + kCancellationPollInterval));
  }
  completion.get();
}

void
ModelStreamerRestore::RestoreFiles(const CheckpointInventory& inventory, const Path& destination, Cancellation cancellation)
{
  for (auto file = inventory.files.begin(); file != inventory.files.end();) {
    auto entry = std::make_shared<StreamerEntry>();
    entry->cancellation = cancellation;
    auto& request = entry->request;
    try {
      while (file != inventory.files.end()) {
        cancellation.ThrowIfCancelled();
        const Path staged = destination / file->relative_path;
        if (file->size_bytes == 0) {
          CreateEmptyFile(*file, staged, cancellation);
          ++file;
          continue;
        }

        if (!entry->TryAddFile(*file, staged))
          break;
        ++file;
      }

      if (!request.paths.empty())
        SubmitAndWait(entry);
    }
    catch (...) {
      // A queued entry can outlive its caller. The admission handshake either
      // prevented submission or waited for successful completion or native
      // teardown before reaching here.
      entry->ReleaseMappings();
      throw;
    }
    entry->ReleaseMappings();
  }
}

void
ModelStreamerRestore::Stage(const CheckpointInventory& inventory, const Path& destination, Cancellation cancellation)
{
  cancellation.ThrowIfCancelled();
  CreateDirectoryTree(inventory, destination, cancellation);
  std::call_once(start_once_, [this] { worker_ = std::thread([this] { Run(); }); });
  RestoreFiles(inventory, destination, cancellation);
  ApplyTreePermissions(inventory, destination, cancellation);
}
}  // namespace snapshot::pagebroker
