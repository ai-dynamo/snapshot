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
#include <semaphore>
#include <stdexcept>
#include <string>
#include <system_error>
#include <unordered_map>
#include <utility>
#include <vector>

#include "file_descriptor.hpp"
#include "model_streamer_api.hpp"
#include "utils/event_loop.hpp"
#include "utils/sha256.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace streamer = model_streamer_api;
namespace {
// Use the Python streamer's default per-submission target: one quarter of its
// 40 GB read-ahead budget. A single larger file is submitted on its own.
constexpr uintmax_t kSubmissionByteBudget = 10'000'000'000;
constexpr unsigned kResponsePollTimeoutMs = 25;
// A completed submission retains its mappings until its entire native session
// ends. Bound admission and stop admitting on the first completion, so steady
// arrivals cannot keep an old destination alive indefinitely.
constexpr std::size_t kSessionSubmissionLimit = 8;

std::binary_semaphore&
NativeSessionGate()
{
  // The pinned S3 plugin cancels and removes clients process-wide at end().
  // Separate coordinators must not overlap native lifetimes. Submissions in
  // the admitted session still run concurrently. A semaphore also permits
  // destructor cleanup on a different thread after the event loop joins.
  static std::binary_semaphore gate(1);
  return gate;
}

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
  const char* description = streamer::runai_file_streamer_response_str(response);
  return description == nullptr ? "unknown Model Streamer error" : description;
}

void
ApplyPermissions(const Path& path, fs::perms permissions)
{
  if (permissions != fs::perms::unknown)
    fs::permissions(path, permissions, fs::perm_options::replace);
}

void
VerifyDigest(std::span<const std::byte> bytes, const std::optional<utils::Sha256Digest>& expected, TransferControl control = {})
{
  if (expected && utils::ComputeSha256(bytes, [&] { control.Check(); }) != *expected)
    throw RestoreIntegrityError();
}

void
ValidateRestorePlan(const RestorePlan& plan)
{
  std::set<Path> directories;
  std::set<Path> entries;
  for (const auto& directory : plan.directories) {
    if (!IsSafeRelativePath(directory.relative_path))
      throw std::invalid_argument("restore plan contains an unsafe directory path");
    const Path parent = directory.relative_path.parent_path();
    if (!parent.empty() && !directories.contains(parent))
      throw std::invalid_argument("restore plan directories must be ordered parent-first");
    if (!directories.insert(directory.relative_path).second || !entries.insert(directory.relative_path).second)
      throw std::invalid_argument("restore plan contains a duplicate path");
  }
  for (const auto& file : plan.files) {
    if (!IsSafeRelativePath(file.relative_path))
      throw std::invalid_argument("restore plan contains an unsafe file path");
    const Path parent = file.relative_path.parent_path();
    if (!parent.empty() && !directories.contains(parent))
      throw std::invalid_argument("restore plan file parent is missing");
    if (!entries.insert(file.relative_path).second)
      throw std::invalid_argument("restore plan contains a duplicate path");
  }
}

class MappedFile {
 public:
  MappedFile(const RestoreFile& file, const Path& destination)
      : source_(file.source_locator), destination_(destination), bytes_(CheckedSize(file.size_bytes)),
        permissions_(file.permissions), expected_sha256_(file.expected_sha256)
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
  void Finish(TransferControl control) const
  {
    VerifyDigest({static_cast<const std::byte*>(address_), bytes_}, expected_sha256_, control);
    ApplyPermissions(destination_, permissions_);
  }

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
  std::optional<utils::Sha256Digest> expected_sha256_;
  void* address_ = nullptr;
};

using MappedFiles = std::vector<std::unique_ptr<MappedFile>>;

void
CreateDirectoryTree(const RestorePlan& plan, const Path& destination)
{
  fs::create_directory(destination);
  for (const auto& directory : plan.directories)
    fs::create_directory(destination / directory.relative_path);
}

void
CreateEmptyFile(const RestoreFile& file, const Path& destination)
{
  FileDescriptor descriptor(open(destination.c_str(), O_CREAT | O_EXCL | O_WRONLY | O_CLOEXEC, 0600));
  if (descriptor.get() < 0)
    throw SystemError("create empty restore staging file");
  // Empty files have no native submission: this verifies their local contents,
  // not the existence of the remote object.
  VerifyDigest({}, file.expected_sha256);
  ApplyPermissions(destination, file.permissions);
}

void
ApplyTreePermissions(const RestorePlan& plan, const Path& destination)
{
  for (auto directory = plan.directories.rbegin(); directory != plan.directories.rend(); ++directory)
    ApplyPermissions(destination / directory->relative_path, directory->permissions);
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
  const int response = streamer::runai_file_streamer_start(&value_);
  if (response != 0) {
    StopStreamer();
    throw std::runtime_error("start Model Streamer: " + StreamerError(response));
  }
  if (value_ == nullptr)
    throw std::runtime_error("Model Streamer started without returning a handle");

  try {
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
      const int status = streamer::runai_file_streamer_set_credentials(
          value_, keys.data(), values.data(), static_cast<unsigned>(keys.size()));
      if (status != 0)
        throw std::runtime_error("configure Model Streamer session: " + StreamerError(status));
    }
  }
  catch (...) {
    StopStreamer();
    throw;
  }
}

void
ModelStreamerRestore::Submit(std::unique_ptr<StreamerEntry>& entry)
{
  pending_.push_back(std::move(entry));
  SubmitPending();
}

bool
ModelStreamerRestore::CanAdmit(const StreamerEntry& entry) const
{
  if (draining_ || active_.size() >= kSessionSubmissionLimit)
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
    if (value_ == nullptr) {
      // Poll admission so queued cancellation/deadlines remain observable
      // while another coordinator drains its native session.
      if (!NativeSessionGate().try_acquire_for(std::chrono::milliseconds(kResponsePollTimeoutMs))) {
        ScheduleReceive();
        return;
      }
      owns_native_session_ = true;
      Start();
    }
    // Keep the entry in pending_ until native submission and registration both
    // finish. On any exception, HandleFailure ends native access before FailAll
    // releases even an entry that the library accepted but we could not track.
    SubmitNative(pending_.front());
    pending_.pop_front();
  }
  // Rejected or cancelled requests must not retain the process-wide permit.
  if (active_.empty())
    StopStreamer();
}

void
ModelStreamerRestore::DiscardInterruptedPending()
{
  for (auto entry = pending_.begin(); entry != pending_.end();) {
    try {
      (*entry)->control.Check();
      ++entry;
    }
    catch (const TransferInterrupted&) {
      FailEntry(**entry, std::current_exception());
      entry = pending_.erase(entry);
    }
  }
}

void
ModelStreamerRestore::SubmitNative(std::unique_ptr<StreamerEntry>& entry)
{
  try {
    entry->control.Check();
  }
  catch (const TransferInterrupted&) {
    // Cancellation can race with session startup or admission. No native
    // request owns this entry yet, so it must not fail other submissions.
    FailEntry(*entry, std::current_exception());
    return;
  }
  auto& request = entry->request;
  streamer::SubmissionId submission_id = 0;
  const int response = streamer::runai_file_streamer_request(
      value_, &submission_id, static_cast<unsigned>(request.paths.size()), request.paths.data(),
      request.range_counts.data(), request.offsets.data(), request.sizes.data(), request.destinations.data(),
      RunaiFileStreamerDevice{RUNAI_FILE_STREAMER_DEVICE_CPU, 0});
  if (response == RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR)
    throw std::runtime_error("submit Model Streamer restore: " + StreamerError(response));
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

  entry->deadline = std::min(entry->control.deadline, std::chrono::steady_clock::now() + submission_timeout_);
  const auto [active, inserted] = active_.try_emplace(submission_id);
  if (!inserted)
    throw std::runtime_error("Model Streamer returned a duplicate submission ID");
  session_bytes_ += entry->bytes;
  ++unfinished_;
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
  if (active_.empty()) {
    SubmitPending();
    ScheduleReceive();
    return;
  }

  StreamerResponse response;
  response.status = streamer::runai_file_streamer_response(
      value_, &response.submission_id, &response.file_index, &response.range_index, &response.submission_done,
      kResponsePollTimeoutMs);
  if (response.status != streamer::kTimedOutStatusCode) {
    if (!IsRecoverableRangeStatus(response.status))
      throw std::runtime_error("Model Streamer session failed: " + StreamerError(response.status));
    const auto entry = active_.find(response.submission_id);
    if (entry == active_.end())
      throw std::runtime_error("Model Streamer returned a response for an unknown submission");
    if (AcceptEntry(*entry->second, response)) {
      --unfinished_;
      draining_ = true;
    }
  }

  const auto now = std::chrono::steady_clock::now();
  for (const auto& [submission_id, entry] : active_) {
    // The native API cannot cancel one submission. Failing the event loop stops
    // the shared streamer before waking every Stage call in HandleFailure.
    entry->control.Check();
    if (now >= entry->deadline)
      throw std::runtime_error("Model Streamer submission " + std::to_string(submission_id) + " timed out");
  }

  DiscardInterruptedPending();
  if (unfinished_ == 0) {
    FinishSession();
    SubmitPending();
  }
  ScheduleReceive();
}

void
ModelStreamerRestore::FinishSession()
{
  StopStreamer();
  for (auto& [id, entry] : active_) {
    if (entry->first_error.empty())
      entry->completion.set_value();
    else
      FailEntry(*entry, std::make_exception_ptr(std::runtime_error(entry->first_error)));
  }
  active_.clear();
  session_bytes_ = 0;
  draining_ = false;
}

void
ModelStreamerRestore::ScheduleReceive()
{
  if (receive_scheduled_)
    return;
  if (active_.empty() && pending_.empty())
    return;

  receive_scheduled_ = true;
  if (!event_loop_.Post(std::make_unique<ReceiveEvent>(*this)))
    receive_scheduled_ = false;
}

void
ModelStreamerRestore::StopStreamer() noexcept
{
  if (value_ != nullptr) {
    streamer::runai_file_streamer_end(value_);
    value_ = nullptr;
  }
  if (owns_native_session_) {
    owns_native_session_ = false;
    NativeSessionGate().release();
  }
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
  for (auto& entry : pending_) {
    if (entry)
      FailEntry(*entry, error);
  }
  active_.clear();
  pending_.clear();
  unfinished_ = 0;
  session_bytes_ = 0;
  draining_ = false;
  receive_scheduled_ = false;
}

void
ModelStreamerRestore::RestoreFiles(const RestorePlan& plan, const Path& destination, TransferControl control)
{
  for (auto file = plan.files.begin(); file != plan.files.end();) {
    MappedFiles batch;
    uintmax_t batch_bytes = 0;
    while (file != plan.files.end()) {
      control.Check();
      const Path staged = destination / file->relative_path;
      if (file->size_bytes == 0) {
        CreateEmptyFile(*file, staged);
        ++file;
        continue;
      }

      if (!batch.empty() &&
          (batch_bytes >= kSubmissionByteBudget || file->size_bytes > kSubmissionByteBudget - batch_bytes))
        break;

      batch.push_back(std::make_unique<MappedFile>(*file, staged));
      batch_bytes += file->size_bytes;
      ++file;
    }

    if (!batch.empty()) {
      if (batch.size() > std::numeric_limits<unsigned>::max())
        throw std::runtime_error("too many files in one Model Streamer submission");

      // The request contains raw pointers into batch. Completion is released
      // only after the native session ends, including on terminal failures.
      auto entry = std::make_unique<StreamerEntry>();
      entry->bytes = batch_bytes;
      entry->control = control;
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

    for (const auto& file : batch) {
      control.Check();
      file->Finish(control);
    }
  }
}

void
ModelStreamerRestore::Stage(const RestorePlan& plan, const Path& destination, TransferControl control)
{
  control.Check();
  ValidateRestorePlan(plan);
  CreateDirectoryTree(plan, destination);
  std::call_once(start_once_, [this] { event_loop_.Start(); });
  RestoreFiles(plan, destination, control);
  control.Check();
  ApplyTreePermissions(plan, destination);
}
}  // namespace snapshot::pagebroker
