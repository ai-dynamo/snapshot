// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_restore.hpp"

#include <fcntl.h>
#include <sys/mman.h>
#include <unistd.h>

#include <algorithm>
#include <cerrno>
#include <chrono>
#include <exception>
#include <filesystem>
#include <future>
#include <limits>
#include <set>
#include <stdexcept>
#include <string>
#include <system_error>
#include <unordered_map>
#include <utility>
#include <vector>

#include "file_descriptor.hpp"
#include <streamer/streamer.h>
#include "utils/event_loop.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
// Use the Python streamer's default per-submission target: one quarter of its
// 40 GB read-ahead budget. A single larger file is submitted on its own.
constexpr uintmax_t kSubmissionByteBudget = 10'000'000'000;
constexpr unsigned kResponsePollTimeoutMs = 25;
constexpr auto kCancellationPollInterval = std::chrono::milliseconds(25);
// Bound each native session even when successful callers return early.
// Failed submissions retain their mappings until the session drains.
constexpr std::size_t kSessionSubmissionLimit = 8;

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
ApplyPermissions(const Path& path, fs::perms permissions)
{
  if (permissions != fs::perms::unknown)
    fs::permissions(path, permissions, fs::perm_options::replace);
}

void
ValidateRestoreInventory(const CheckpointInventory& inventory)
{
  std::set<Path> directories;
  std::set<Path> entries;
  for (const auto& directory : inventory.directories) {
    if (!IsSafeRelativePath(directory.relative_path))
      throw std::invalid_argument("restore inventory contains an unsafe directory path");
    const Path parent = directory.relative_path.parent_path();
    if (!parent.empty() && !directories.contains(parent))
      throw std::invalid_argument("restore inventory directories must be ordered parent-first");
    if (!directories.insert(directory.relative_path).second || !entries.insert(directory.relative_path).second)
      throw std::invalid_argument("restore inventory contains a duplicate path");
  }
  for (const auto& file : inventory.files) {
    // Source locators are remote object addresses; never interpret them as local paths.
    const auto& locator = file.source_locator;
    const auto slash = locator.find('/', 5);
    if (!locator.starts_with("s3://") || locator.find('\0') != std::string::npos ||
        slash == std::string::npos || slash == 5 || slash + 1 == locator.size())
      throw std::invalid_argument("Model Streamer requires an S3 object source");
    if (!IsSafeRelativePath(file.relative_path))
      throw std::invalid_argument("restore inventory contains an unsafe file path");
    const Path parent = file.relative_path.parent_path();
    if (!parent.empty() && !directories.contains(parent))
      throw std::invalid_argument("restore inventory file parent is missing");
    if (!entries.insert(file.relative_path).second)
      throw std::invalid_argument("restore inventory contains a duplicate path");
  }
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
CreateDirectoryTree(const CheckpointInventory& inventory, const Path& destination)
{
  fs::create_directory(destination);
  for (const auto& directory : inventory.directories)
    fs::create_directory(destination / directory.relative_path);
}

void
CreateEmptyFile(const CheckpointFile& file, const Path& destination)
{
  FileDescriptor descriptor(open(destination.c_str(), O_CREAT | O_EXCL | O_WRONLY | O_CLOEXEC, 0600));
  if (descriptor.get() < 0)
    throw SystemError("create empty restore staging file");
  // Empty files need no native submission; the backend checks remote existence.
  ApplyPermissions(destination, file.permissions);
}

void
ApplyTreePermissions(const CheckpointInventory& inventory, const Path& destination)
{
  for (auto directory = inventory.directories.rbegin(); directory != inventory.directories.rend(); ++directory)
    ApplyPermissions(destination / directory->relative_path, directory->permissions);
  ApplyPermissions(destination, inventory.root_permissions);
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
  void* address = mmap(nullptr, file_bytes, PROT_READ | PROT_WRITE, MAP_SHARED, descriptor.get(), 0);
  if (address == MAP_FAILED)
    throw SystemError("map restore staging file");
  request.destinations.back() = address;
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

// The pinned Model Streamer ABI permits multiple in-flight submissions but
// does not permit request and response calls in parallel. Restore events are
// therefore the sole API callers, while request threads wait on futures for
// their independently tracked submissions.
ModelStreamerRestore::SubmitEvent::SubmitEvent(
    ModelStreamerRestore& restore,
    std::shared_ptr<StreamerEntry> entry)
    : restore_(restore), entry_(std::move(entry))
{
}

void
ModelStreamerRestore::SubmitEvent::Execute()
{
  restore_.QueueSubmission(entry_);
}

void
ModelStreamerRestore::SubmitEvent::Cancel(std::exception_ptr error) noexcept
{
  if (entry_)
    restore_.FailEntry(*entry_, std::move(error));
}

ModelStreamerRestore::WorkEvent::WorkEvent(ModelStreamerRestore& restore)
    : restore_(restore)
{
}

void
ModelStreamerRestore::WorkEvent::Execute()
{
  restore_.ProcessWork();
}

void
ModelStreamerRestore::WorkEvent::Cancel(std::exception_ptr) noexcept
{
}

ModelStreamerRestore::ModelStreamerRestore(std::chrono::milliseconds submission_timeout)
    : ModelStreamerRestore(ModelStreamerSessionOptions{}, submission_timeout)
{
}

ModelStreamerRestore::ModelStreamerRestore(
    ModelStreamerSessionOptions options,
    std::chrono::milliseconds submission_timeout)
    : submission_timeout_(submission_timeout), options_(std::move(options)),
      stopped_error_(std::make_exception_ptr(std::runtime_error("Model Streamer restore stopped"))),
      event_loop_([this](std::exception_ptr error) { HandleFailure(std::move(error)); })
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
  event_loop_.Stop();
  // A stopped event loop can leave accepted submissions active. Model
  // Streamer must release their raw destinations before their waiters wake.
  StopStreamer();
  FailAll(stopped_error_);
}

bool
ModelStreamerRestore::Failed() const noexcept
{
  return failed_.load(std::memory_order_acquire);
}

void
ModelStreamerRestore::Start()
{
  const int response = runai_file_streamer_start(&value_);
  if (response != 0)
    throw std::runtime_error("start Model Streamer: " + StreamerError(response));
  if (value_ == nullptr)
    throw std::runtime_error("Model Streamer started without returning a handle");

  // The event-loop failure handler owns teardown, including partial startup.
  std::vector<const char*> keys;
  std::vector<const char*> values;
  const auto append = [&](const char* key, const std::string& value) {
    if (!value.empty()) {
      keys.push_back(key);
      values.push_back(value.c_str());
    }
  };
  append("region", options_.region);
  append("endpoint", options_.endpoint);
  append("access_key_id", options_.access_key_id);
  append("secret_access_key", options_.secret_access_key);
  append("session_token", options_.session_token);
  if (!keys.empty()) {
    const int status = runai_file_streamer_set_credentials(
        value_, keys.data(), values.data(), static_cast<unsigned>(keys.size()));
    if (status != 0)
      throw std::runtime_error("configure Model Streamer session: " + StreamerError(status));
  }
}

void
ModelStreamerRestore::QueueSubmission(std::shared_ptr<StreamerEntry>& entry)
{
  pending_.push_back(std::move(entry));
  ScheduleWork();
}

bool
ModelStreamerRestore::CanAdmit(const StreamerEntry& entry) const
{
  // Successful entries have left active_. A completed entry still here failed;
  // stop admission so the session can drain and release its mappings.
  if (unfinished_ < active_.size() || session_submissions_ >= kSessionSubmissionLimit)
    return false;
  // An oversized file may occupy a session by itself.
  if (active_.empty())
    return true;
  if (session_bytes_ >= kSubmissionByteBudget)
    return false;
  return entry.bytes <= kSubmissionByteBudget - session_bytes_;
}

void
ModelStreamerRestore::SubmitPending()
{
  DiscardInterruptedPending();
  while (!pending_.empty() && CanAdmit(*pending_.front())) {
    if (value_ == nullptr)
      Start();
    // Keep the entry in pending_ until native submission and registration both
    // finish. On any exception, HandleFailure ends native access before FailAll
    // releases even an entry that the library accepted but we could not track.
    AdmitAndSubmit(pending_.front());
    pending_.pop_front();
  }
  // End an unused session if every request was rejected or cancelled.
  if (active_.empty())
    StopStreamer();
}

void
ModelStreamerRestore::DiscardInterruptedPending()
{
  for (auto entry = pending_.begin(); entry != pending_.end();) {
    try {
      (*entry)->cancellation.ThrowIfCancelled();
      ++entry;
    }
    catch (const CancellationError&) {
      FailEntry(**entry, std::current_exception());
      entry = pending_.erase(entry);
    }
  }
}

void
ModelStreamerRestore::AdmitAndSubmit(std::shared_ptr<StreamerEntry>& entry)
{
  try {
    entry->cancellation.ThrowIfCancelled();
  }
  catch (const CancellationError&) {
    // Cancellation can race with session startup or admission. No native
    // request owns this entry yet, so it must not fail other submissions.
    FailEntry(*entry, std::current_exception());
    return;
  }
  // Let the caller cancel while the session starts. Claim the entry just before
  // submitting it, so its buffers stay alive while Model Streamer uses them.
  auto expected = Admission::Pending;
  if (!entry->admission.compare_exchange_strong(expected, Admission::Claimed))
    return;  // The caller cancelled and may already have released its buffers.

  const auto submission_id = SubmitToStreamer(*entry);
  if (submission_id == 0)
    return;

  entry->deadline = std::min(entry->cancellation.Deadline(), std::chrono::steady_clock::now() + submission_timeout_);
  const auto [active, inserted] = active_.try_emplace(submission_id);
  if (!inserted)
    throw std::runtime_error("Model Streamer returned a duplicate submission ID");
  session_bytes_ += entry->bytes;
  ++session_submissions_;
  ++unfinished_;
  active->second = std::move(entry);
}

std::uint64_t
ModelStreamerRestore::SubmitToStreamer(StreamerEntry& entry)
{
  auto& request = entry.request;
  RunaiFileStreamerSubmissionId submission_id = 0;
  const int response = runai_file_streamer_request(
      value_, &submission_id, static_cast<unsigned>(request.paths.size()), request.paths.data(),
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
    if (const auto reason = entry.cancellation.Reason())
      throw CancellationError(*reason);
    try {
      std::rethrow_exception(error);
    }
    catch (const CancellationError&) {
      throw std::runtime_error("Model Streamer session interrupted by another submission's cancellation");
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
  work_scheduled_ = false;
  SubmitPending();
  if (active_.empty())
    return;

  StreamerResponse response;
  std::shared_ptr<StreamerEntry> completed;
  response.status = runai_file_streamer_response(
      value_, &response.submission_id, &response.file_index, &response.range_index, &response.submission_done,
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
    // The native API cannot cancel one submission. Failing the event loop stops
    // the shared streamer before waking every Stage call in HandleFailure.
    entry->cancellation.ThrowIfCancelled();
    if (now >= entry->deadline)
      throw std::runtime_error("Model Streamer submission " + std::to_string(submission_id) + " timed out");
  }

  if (completed) {
    // Every CPU/S3 range succeeded: native access to this entry has ended,
    // independently of other submissions. Remove it before waking its caller,
    // which may immediately release the mappings and the inventory's source strings.
    active_.erase(response.submission_id);
    completed->completion.set_value();
  }

  if (unfinished_ == 0)
    FinishSession();
  ScheduleWork();
}

void
ModelStreamerRestore::FinishSession()
{
  StopStreamer();
  for (auto& [id, entry] : active_) {
    FailEntry(*entry, std::make_exception_ptr(std::runtime_error(entry->first_error)));
  }
  active_.clear();
  session_bytes_ = 0;
  session_submissions_ = 0;
}

void
ModelStreamerRestore::ScheduleWork()
{
  if (work_scheduled_)
    return;
  if (active_.empty() && pending_.empty())
    return;

  work_scheduled_ = true;
  if (!event_loop_.Post(std::make_unique<WorkEvent>(*this)))
    work_scheduled_ = false;
}

void
ModelStreamerRestore::StopStreamer() noexcept
{
  if (value_ != nullptr) {
    runai_file_streamer_end(value_);
    value_ = nullptr;
  }
}

void
ModelStreamerRestore::HandleFailure(std::exception_ptr error) noexcept
{
  // Model Streamer may still hold raw mapped destinations. Stop it before
  // waking callers who will then release their entries' mappings.
  StopStreamer();
  failed_.store(true, std::memory_order_release);
  FailAll(std::move(error));
}

void
ModelStreamerRestore::FailAll(std::exception_ptr error) noexcept
{
  for (auto& [id, entry] : active_)
    FailEntry(*entry, error);
  for (auto& entry : pending_) {
    if (entry)
      FailEntry(*entry, error);
  }
  active_.clear();
  pending_.clear();
  unfinished_ = 0;
  session_bytes_ = 0;
  session_submissions_ = 0;
  work_scheduled_ = false;
}

void
ModelStreamerRestore::SubmitAndWait(const std::shared_ptr<StreamerEntry>& entry)
{
  auto completion = entry->completion.get_future();
  event_loop_.Post(std::make_unique<SubmitEvent>(*this, entry));
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
    const auto batch_begin = file;
    auto entry = std::make_shared<StreamerEntry>();
    entry->cancellation = cancellation;
    auto& request = entry->request;
    try {
      while (file != inventory.files.end()) {
        cancellation.ThrowIfCancelled();
        const Path staged = destination / file->relative_path;
        if (file->size_bytes == 0) {
          CreateEmptyFile(*file, staged);
          ++file;
          continue;
        }

        if (!entry->TryAddFile(*file, staged))
          break;
        ++file;
      }

      if (!request.paths.empty())
        SubmitAndWait(entry);

      for (auto restored = batch_begin; restored != file; ++restored) {
        if (restored->size_bytes == 0)
          continue;  // Empty files already received their permissions on creation.
        cancellation.ThrowIfCancelled();
        ApplyPermissions(destination / restored->relative_path, restored->permissions);
      }
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
  ValidateRestoreInventory(inventory);
  CreateDirectoryTree(inventory, destination);
  std::call_once(start_once_, [this] { event_loop_.Start(); });
  RestoreFiles(inventory, destination, cancellation);
  cancellation.ThrowIfCancelled();
  ApplyTreePermissions(inventory, destination);
}
}  // namespace snapshot::pagebroker
