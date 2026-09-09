// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_restore.hpp"

#include <fcntl.h>
#include <sys/mman.h>
#include <unistd.h>

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
#include "model_streamer_api.hpp"
#include "utils/event_loop.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace streamer = model_streamer_api;
namespace {
// Use the Python streamer's default per-submission target: one quarter of its
// 40 GB read-ahead budget. A single larger file is submitted on its own.
constexpr uintmax_t kSubmissionByteBudget = 10'000'000'000;
constexpr unsigned kResponsePollTimeoutMs = 25;

std::system_error
SystemError(const char* operation)
{
  return std::system_error(errno, std::generic_category(), operation);
}

std::string
StreamerError(int response)
{
  const char* description = streamer::runai_response_str(response);
  return description == nullptr ? "unknown Model Streamer error" : description;
}

void
ApplyPermissions(const Path& path, fs::perms permissions)
{
  if (permissions != fs::perms::unknown)
    fs::permissions(path, permissions, fs::perm_options::replace);
}

bool
IsSafeRelativePath(const Path& path)
{
  if (path.empty() || path.is_absolute() || path.has_root_name() || path.has_root_directory() ||
      path.lexically_normal() != path)
    return false;
  for (const auto& component : path) {
    if (component.empty() || component == "." || component == "..")
      return false;
  }
  return true;
}

void
ValidateRestorePlan(const RestorePlan& plan)
{
  std::set<Path> directories;
  std::set<Path> entries;
  for (const auto& directory : plan.directories) {
    if (!IsSafeRelativePath(directory.relative))
      throw std::invalid_argument("restore plan contains an unsafe directory path");
    const Path parent = directory.relative.parent_path();
    if (!parent.empty() && !directories.contains(parent))
      throw std::invalid_argument("restore plan directories must be ordered parent-first");
    if (!directories.insert(directory.relative).second || !entries.insert(directory.relative).second)
      throw std::invalid_argument("restore plan contains a duplicate path");
  }
  for (const auto& file : plan.files) {
    if (!IsSafeRelativePath(file.relative))
      throw std::invalid_argument("restore plan contains an unsafe file path");
    const Path parent = file.relative.parent_path();
    if (!parent.empty() && !directories.contains(parent))
      throw std::invalid_argument("restore plan file parent is missing");
    if (!entries.insert(file.relative).second)
      throw std::invalid_argument("restore plan contains a duplicate path");
  }
}

class MappedFile {
 public:
  MappedFile(const RestoreFile& file, const Path& destination)
      : source_(file.source_locator), destination_(destination), bytes_(CheckedSize(file.bytes)),
        permissions_(file.permissions)
  {
    FileDescriptor descriptor(open(destination_.c_str(), O_CREAT | O_EXCL | O_RDWR | O_CLOEXEC, 0600));
    if (descriptor.get() < 0)
      throw SystemError("create restore staging file");
    if (ftruncate(descriptor.get(), static_cast<off_t>(bytes_)) < 0)
      throw SystemError("size restore staging file");
    address_ = mmap(nullptr, bytes_, PROT_READ | PROT_WRITE, MAP_SHARED, descriptor.get(), 0);
    if (address_ == MAP_FAILED) {
      address_ = nullptr;
      throw SystemError("map restore staging file");
    }
  }

  MappedFile(const MappedFile&) = delete;
  MappedFile& operator=(const MappedFile&) = delete;

  ~MappedFile() noexcept
  {
    if (address_ != nullptr)
      munmap(address_, bytes_);
  }

  const std::string& source() const { return source_; }
  void* address() const { return address_; }
  size_t bytes() const { return bytes_; }
  void Finish() const { ApplyPermissions(destination_, permissions_); }

 private:
  static size_t CheckedSize(uintmax_t bytes)
  {
    if (bytes == 0 || bytes > std::numeric_limits<size_t>::max() ||
        bytes > static_cast<uintmax_t>(std::numeric_limits<off_t>::max()))
      throw std::runtime_error("invalid restore file size");
    return static_cast<size_t>(bytes);
  }

  std::string source_;
  Path destination_;
  size_t bytes_;
  fs::perms permissions_;
  void* address_ = nullptr;
};

using MappedFiles = std::vector<std::unique_ptr<MappedFile>>;

void
CreateDirectoryTree(const RestorePlan& plan, const Path& destination)
{
  fs::create_directory(destination);
  for (const auto& directory : plan.directories)
    fs::create_directory(destination / directory.relative);
}

void
CreateEmptyFile(const RestoreFile& file, const Path& destination)
{
  FileDescriptor descriptor(open(destination.c_str(), O_CREAT | O_EXCL | O_WRONLY | O_CLOEXEC, 0600));
  if (descriptor.get() < 0)
    throw SystemError("create empty restore staging file");
  ApplyPermissions(destination, file.permissions);
}

void
ApplyTreePermissions(const RestorePlan& plan, const Path& destination)
{
  for (auto directory = plan.directories.rbegin(); directory != plan.directories.rend(); ++directory)
    ApplyPermissions(destination / directory->relative, directory->permissions);
  ApplyPermissions(destination, plan.root_permissions);
}
}  // namespace

// The pinned Model Streamer ABI permits multiple in-flight submissions but
// does not permit request and response calls in parallel. Restore events are
// therefore the sole API callers, while request threads wait on futures for
// their independently tracked submissions.
ModelStreamerRestore::SubmitEvent::SubmitEvent(
    ModelStreamerRestore& restore,
    std::unique_ptr<StreamerEntry> entry)
    : restore_(restore), entry_(std::move(entry))
{
}

void
ModelStreamerRestore::SubmitEvent::Execute()
{
  restore_.Submit(entry_);
}

void
ModelStreamerRestore::SubmitEvent::Cancel(std::exception_ptr error) noexcept
{
  if (entry_)
    restore_.FailEntry(*entry_, std::move(error));
}

ModelStreamerRestore::ReceiveEvent::ReceiveEvent(ModelStreamerRestore& restore)
    : restore_(restore)
{
}

void
ModelStreamerRestore::ReceiveEvent::Execute()
{
  restore_.ReceiveAndDispatch();
}

void
ModelStreamerRestore::ReceiveEvent::Cancel(std::exception_ptr) noexcept
{
}

ModelStreamerRestore::ModelStreamerRestore(std::chrono::milliseconds submission_timeout)
    : submission_timeout_(submission_timeout),
      stopped_error_(std::make_exception_ptr(std::runtime_error("Model Streamer restore stopped"))),
      event_loop_([this](std::exception_ptr error) { HandleFailure(std::move(error)); })
{
  if (submission_timeout_ <= std::chrono::milliseconds::zero())
    throw std::invalid_argument("Model Streamer submission timeout must be positive");
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
  const int response = streamer::runai_start(&value_);
  if (response != 0) {
    StopStreamer();
    throw std::runtime_error("start Model Streamer: " + StreamerError(response));
  }
  if (value_ == nullptr)
    throw std::runtime_error("Model Streamer started without returning a handle");

  try {
    event_loop_.Start();
  }
  catch (...) {
    StopStreamer();
    throw;
  }
}

void
ModelStreamerRestore::Submit(std::unique_ptr<StreamerEntry>& entry)
{
  auto& request = entry->request;
  streamer::SubmissionId submission_id = 0;
  const int response = streamer::runai_request(
      value_, &submission_id, static_cast<unsigned>(request.paths.size()), request.paths.data(),
      request.range_counts.data(), request.offsets.data(), request.sizes.data(), request.destinations.data());
  if (response != 0 && submission_id == 0) {
    FailEntry(
        *entry,
        std::make_exception_ptr(std::runtime_error("submit Model Streamer restore: " + StreamerError(response))));
    return;
  }
  if (response != 0)
    throw std::runtime_error("submit Model Streamer restore: " + StreamerError(response));
  if (submission_id == 0)
    throw std::runtime_error("Model Streamer accepted a submission without assigning an ID");

  entry->deadline = std::chrono::steady_clock::now() + submission_timeout_;
  const auto [active, inserted] = active_.try_emplace(submission_id);
  if (!inserted)
    throw std::runtime_error("Model Streamer returned a duplicate submission ID");
  active->second = std::move(entry);
  ScheduleReceive();
}

bool
ModelStreamerRestore::AcceptEntry(StreamerEntry& entry, const StreamerResponse& response)
{
  if (entry.responses_received >= entry.completed.size())
    throw std::runtime_error("Model Streamer returned too many responses for a submission");

  if (response.status != 0 && entry.first_error.empty())
    entry.first_error = "Model Streamer restore range: " + StreamerError(response.status);

  if (response.file_index >= entry.completed.size() || response.range_index != 0 ||
      (response.file_index < entry.completed.size() && entry.completed[response.file_index])) {
    if (entry.first_error.empty())
      entry.first_error = "Model Streamer returned an invalid or duplicate range response";
  } else {
    entry.completed[response.file_index] = true;
  }

  ++entry.responses_received;
  const bool expected_done = entry.responses_received == entry.completed.size();
  if ((response.submission_done != 0) != expected_done && entry.first_error.empty())
    entry.first_error = "Model Streamer reported inconsistent submission completion";

  if (response.submission_done != 0) {
    if (entry.first_error.empty()) {
      entry.completion.set_value();
    } else {
      entry.completion.set_exception(std::make_exception_ptr(std::runtime_error(entry.first_error)));
    }
    return true;
  }
  if (expected_done)
    throw std::runtime_error("Model Streamer did not finish a fully drained submission");
  return false;
}

void
ModelStreamerRestore::FailEntry(StreamerEntry& entry, std::exception_ptr error) noexcept
{
  try {
    entry.completion.set_exception(std::move(error));
  }
  catch (...) {
  }
}

void
ModelStreamerRestore::ReceiveAndDispatch()
{
  receive_scheduled_ = false;
  if (active_.empty())
    return;

  StreamerResponse response;
  response.status = streamer::runai_response(
      value_, &response.submission_id, &response.file_index, &response.range_index, &response.submission_done,
      kResponsePollTimeoutMs);
  if (response.status != streamer::kTimedOut) {
    const auto entry = active_.find(response.submission_id);
    if (entry == active_.end())
      throw std::runtime_error("Model Streamer returned a response for an unknown submission");
    if (AcceptEntry(*entry->second, response))
      active_.erase(entry);
  }

  const auto now = std::chrono::steady_clock::now();
  for (const auto& [submission_id, entry] : active_) {
    // The native API cannot cancel one submission. Failing the event loop stops
    // the shared streamer before waking every Stage call in HandleFailure.
    if (now >= entry->deadline)
      throw std::runtime_error("Model Streamer submission " + std::to_string(submission_id) + " timed out");
  }

  ScheduleReceive();
}

void
ModelStreamerRestore::ScheduleReceive()
{
  if (receive_scheduled_ || active_.empty())
    return;

  receive_scheduled_ = true;
  if (!event_loop_.Post(std::make_unique<ReceiveEvent>(*this)))
    receive_scheduled_ = false;
}

void
ModelStreamerRestore::StopStreamer() noexcept
{
  if (value_ == nullptr)
    return;
  streamer::runai_end(value_);
  value_ = nullptr;
}

void
ModelStreamerRestore::HandleFailure(std::exception_ptr error) noexcept
{
  // Model Streamer may still hold raw mapped destinations. Stop it before
  // waking callers whose stack-owned mappings will then be released.
  StopStreamer();
  failed_.store(true, std::memory_order_release);
  FailAll(std::move(error));
}

void
ModelStreamerRestore::FailAll(std::exception_ptr error) noexcept
{
  for (auto& [id, entry] : active_)
    FailEntry(*entry, error);
  active_.clear();
  receive_scheduled_ = false;
}

void
ModelStreamerRestore::RestoreFiles(const RestorePlan& plan, const Path& destination)
{
  for (auto file = plan.files.begin(); file != plan.files.end();) {
    MappedFiles batch;
    uintmax_t batch_bytes = 0;
    while (file != plan.files.end()) {
      const Path staged = destination / file->relative;
      if (file->bytes == 0) {
        CreateEmptyFile(*file, staged);
        ++file;
        continue;
      }

      if (!batch.empty() &&
          (batch_bytes >= kSubmissionByteBudget || file->bytes > kSubmissionByteBudget - batch_bytes))
        break;

      batch.push_back(std::make_unique<MappedFile>(*file, staged));
      batch_bytes += file->bytes;
      ++file;
    }

    if (!batch.empty()) {
      if (batch.size() > std::numeric_limits<unsigned>::max())
        throw std::runtime_error("too many files in one Model Streamer submission");

      // The request contains raw pointers into batch. Waiting here keeps the
      // mappings alive until the receive event sees submission_done.
      auto entry = std::make_unique<StreamerEntry>();
      entry->request.paths.reserve(batch.size());
      entry->request.range_counts.assign(batch.size(), 1);
      entry->request.offsets.assign(batch.size(), 0);
      entry->request.sizes.reserve(batch.size());
      entry->request.destinations.reserve(batch.size());
      entry->completed.resize(batch.size(), false);
      for (const auto& file : batch) {
        entry->request.paths.push_back(file->source().c_str());
        entry->request.sizes.push_back(file->bytes());
        entry->request.destinations.push_back(file->address());
      }
      auto completion = entry->completion.get_future();
      event_loop_.Post(std::make_unique<SubmitEvent>(*this, std::move(entry)));
      completion.get();
    }

    for (const auto& file : batch)
      file->Finish();
  }
}

void
ModelStreamerRestore::Stage(const RestorePlan& plan, const Path& destination)
{
  ValidateRestorePlan(plan);
  CreateDirectoryTree(plan, destination);
  std::call_once(start_once_, [this] { Start(); });
  RestoreFiles(plan, destination);
  ApplyTreePermissions(plan, destination);
}
}  // namespace snapshot::pagebroker
