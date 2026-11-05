// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"

#include <gtest/gtest.h>
#include <fcntl.h>
#include <sys/mman.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <deque>
#include <filesystem>
#include <fstream>
#include <future>
#include <map>
#include <mutex>
#include <new>
#include <optional>
#include <string>
#include <system_error>
#include <sys/stat.h>
#include <thread>
#include <tuple>
#include <unistd.h>
#include <vector>

#include <streamer/streamer.h>

#include "tests/temporary_directory.hpp"

using namespace snapshot::pagebroker;
using namespace std::chrono_literals;
namespace fs = std::filesystem;

namespace {
model_streamer::settings::Options
RestoreSettings(std::uint64_t batch_bytes, std::optional<std::uint64_t> preparation_slots = std::nullopt)
{
  model_streamer::settings::Options settings;
  settings.restore_batch_bytes = batch_bytes;
  settings.restore_preparation_slots = preparation_slots;
  return settings;
}

// The session binary alone injects allocation failure; dependencies stay intact.
thread_local bool fail_next_allocation = false;
thread_local std::size_t fail_pending_allocation_size = 0;
thread_local std::size_t last_failed_allocation_size = 0;
thread_local bool record_allocation_size = false;
thread_local std::size_t last_allocation_size = 0;
thread_local std::size_t fail_allocation_size = 0;
thread_local unsigned skip_matching_allocations = 0;
void HoldActiveFrontForQueueGrowthFailure();
thread_local ModelStreamerRestore* producer_restore = nullptr;
}

extern "C" void* __real__Znwm(std::size_t size);
extern "C" void* __wrap__Znwm(std::size_t size)
{
  if (fail_next_allocation || (fail_pending_allocation_size != 0 && size == fail_pending_allocation_size)) {
    fail_next_allocation = false;
    fail_pending_allocation_size = 0;
    last_failed_allocation_size = size;
    throw std::bad_alloc();
  }
  if (record_allocation_size)
    last_allocation_size = size;
  if (fail_allocation_size != 0 && size == fail_allocation_size) {
    if (skip_matching_allocations != 0) {
      --skip_matching_allocations;
    }
    else {
      fail_allocation_size = 0;
      HoldActiveFrontForQueueGrowthFailure();
      throw std::bad_alloc();
    }
  }
  return __real__Znwm(size);
}

namespace snapshot::pagebroker {
class ModelStreamerRestoreTestPeer {
 public:
  // Check actual acceptance while a native barrier holds the worker. try_lock
  // makes an accidentally held queue mutex fail the test instead of hanging it.
  static bool WaitForPending(ModelStreamerRestore& restore, std::size_t count)
  {
    const auto deadline = std::chrono::steady_clock::now() + 5s;
    do {
      {
        std::unique_lock lock(restore.pending_mutex_, std::try_to_lock);
        if (lock.owns_lock() && restore.pending_.size() == count)
          return true;
      }
      std::this_thread::sleep_for(1ms);
    } while (std::chrono::steady_clock::now() < deadline);
    return false;
  }

  // Discover the allocation made by the public deque API rather than pinning
  // a library-specific block size. Empty shared pointers allocate no entries.
  static std::size_t LocalQueueBlockAllocationSize()
  {
    last_allocation_size = 0;
    record_allocation_size = true;
    {
      std::deque<std::shared_ptr<ModelStreamerRestore::StreamerEntry>> queue;
      queue.emplace_back();
    }
    record_allocation_size = false;
    return last_allocation_size;
  }
  static bool HasWorker(const ModelStreamerRestore& restore) { return restore.worker_.joinable(); }
  static void RequestStop(ModelStreamerRestore& restore) { restore.RequestStop(); }
  // Called after a terminal native failure reached end(); establishes that all
  // failure promises have been fulfilled before resuming a held producer.
  static void JoinFailedWorker(ModelStreamerRestore& restore) { restore.worker_.join(); }
  static std::future<void> QueueCancelledBarrier(ModelStreamerRestore& restore)
  {
    auto entry = std::make_shared<ModelStreamerRestore::StreamerEntry>();
    entry->cancellation.Cancel();
    auto completion = entry->completion.get_future();
    restore.QueueSubmission(entry);
    return completion;
  }
  static std::size_t PendingCount(ModelStreamerRestore& restore)
  {
    std::lock_guard lock(restore.pending_mutex_);
    return restore.pending_.size();
  }
  static std::future<void> QueueFailedBatch(ModelStreamerRestore& restore, uintmax_t bytes)
  {
    auto entry = std::make_shared<ModelStreamerRestore::StreamerEntry>();
    entry->bytes = bytes;
    entry->restore_state = std::make_shared<ModelStreamerRestore::RestoreState>();
    entry->restore_state->error = std::make_exception_ptr(std::runtime_error("earlier batch failed"));
    auto completion = entry->completion.get_future();
    restore.QueueSubmission(entry);
    return completion;
  }
  static bool HasOnlyUnclaimedPending(ModelStreamerRestore& restore, std::size_t count = 1)
  {
    std::lock_guard lock(restore.pending_mutex_);
    return restore.pending_.size() == count && std::all_of(restore.pending_.begin(), restore.pending_.end(),
        [](const auto& entry) { return entry->admission.load() == ModelStreamerRestore::Admission::Pending; });
  }
  static bool AllPendingWithdrawn(ModelStreamerRestore& restore)
  {
    std::lock_guard lock(restore.pending_mutex_);
    return !restore.pending_.empty() && std::all_of(restore.pending_.begin(), restore.pending_.end(),
        [](const auto& entry) { return entry->admission.load() == ModelStreamerRestore::Admission::Cancelled; });
  }
  static void ExpectQueueAllocationFailure(ModelStreamerRestore& restore, bool retain = false)
  {
    // Fill the real deque until push_back must allocate. The worker must be
    // absent or held in a native request throughout this test-only operation.
    bool failed = false;
    for (unsigned i = 0; i < 256 && !failed; ++i) {
      auto entry = std::make_shared<ModelStreamerRestore::StreamerEntry>();
      entry->admission = ModelStreamerRestore::Admission::Cancelled;
      auto completion = entry->completion.get_future();
      const auto before = restore.pending_.size();
      fail_next_allocation = true;
      restore.QueueSubmission(entry);
      fail_next_allocation = false;
      if (completion.wait_for(0ms) == std::future_status::ready) {
        EXPECT_THROW(completion.get(), std::bad_alloc);
        EXPECT_EQ(restore.pending_.size(), before);
        failed = true;
      }
    }
    EXPECT_TRUE(failed);
    if (!retain)
      restore.pending_.clear();
  }
};
}  // namespace snapshot::pagebroker

namespace {
enum class Fault { NONE, UNKNOWN, INVALID_INDEX, EARLY_DONE, MISSING_DONE, STORAGE, TIMEOUT };
struct Response {
  RunaiFileStreamerSubmissionId id;
  unsigned file;
  int done;
};
struct Session {
  struct Submission {
    std::vector<char*> destinations;
    // Native readers borrow the actual ABI arrays and inventory strings.
    std::vector<const char* const*> paths;
    std::vector<const std::size_t*> sizes;
    std::vector<void* const*> destination_slots;
    std::vector<std::string> sources;
    bool failed = false;
    uintmax_t bytes = 0;
  };
  std::vector<Response> responses;
  std::map<RunaiFileStreamerSubmissionId, Submission> entries;
  std::size_t next = 0;
};
struct FakeState {
  std::mutex mutex;
  std::condition_variable changed;
  bool hold_end = false;
  unsigned hold_end_after_submission = 0;
  bool hold_start = false;
  bool hold_request = false;
  bool allow_responses = true;
  bool pause_after_completion = false;
  bool pause_after_response = false;
  unsigned completed_submissions = 0;
  unsigned allocation_calls = 0;
  unsigned hold_allocation = 0;
  unsigned allocation_entered = 0;
  unsigned fail_allocation = 0;
  unsigned fail_mapping = 0;
  bool hold_producer = false;
  unsigned producer_window_slots = 2;
  bool hold_producer_with_pending_front = false;
  bool producer_at_window = false;
  bool arm_queue_failure = false;
  std::size_t queue_allocation_bytes = 0;
  bool arm_slot_failure = false;
  bool queue_growth_failed = false;
  bool fail_registration = false;
  int allocation_error = 0;
  Cancellation preparation_cancellation;
  Path cancel_after_directory;
  Path cancel_after_permissions;
  bool cancel_after_allocation = false;
  unsigned mappings = 0;
  int allocated_descriptor = -1;
  std::vector<Path> permission_paths;
  unsigned permission_calls = 0;
  unsigned fail_permission_call = 0;
  int start_status = 0;
  bool return_start_handle = true;
  int credential_status = 0;
  int request_status = RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
  bool return_submission_id = true;
  RunaiFileStreamerSubmissionId storage_failure_submission = 0;
  bool sparse_allocations = false;
  unsigned starts = 0;
  unsigned allocations = 0;
  unsigned requests_entered = 0;
  unsigned reject_request = 0;
  unsigned response_polls = 0;
  unsigned ends_entered = 0;
  unsigned ends = 0;
  unsigned submitted = 0;
  std::size_t max_active_submissions = 0;
  uintmax_t max_active_bytes = 0;
  std::vector<std::string> sources;
  std::vector<std::vector<std::string>> submission_sources;
  std::vector<uintmax_t> submission_bytes;
  std::thread::id native_thread;
  Fault fault = Fault::NONE;
  RunaiFileStreamerSubmissionId next_id = 0;
} state;

// A producer's node allocation happens after publishing its front. The test
// releases one response at a time and waits for the next preparation, so this
// front cannot complete before the injected failure and teardown assertions.
void HoldActiveFrontForQueueGrowthFailure()
{
  auto* producer = producer_restore;
  producer_restore = nullptr;  // Clock wrapping must not relock state.mutex.
  {
    std::unique_lock lock(state.mutex);
    state.allow_responses = false;
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] {
      return state.submitted > state.completed_submissions;
    }));
    state.queue_growth_failed = true;
    state.changed.notify_all();
  }
  producer_restore = producer;
}

// Inspect the actual mappings, including mappings of files already unlinked by
// caller cleanup. This also catches resources retained by a queued shared entry.
bool HasMapping(const Path& path)
{
  std::ifstream maps("/proc/self/maps");
  if (!maps.is_open()) {
    ADD_FAILURE() << "cannot inspect process mappings";
    return true;
  }
  for (std::string line; std::getline(maps, line);) {
    if (line.find(path.string()) != std::string::npos)
      return true;
  }
  return false;
}

// Tests run sequentially. Declare before the coordinator so its worker joins
// before the process-wide mask is restored, including on assertion failure.
class ScopedUmask {
 public:
  explicit ScopedUmask(mode_t mask) : previous_(umask(mask)) {}
  ~ScopedUmask() { umask(previous_); }
  ScopedUmask(const ScopedUmask&) = delete;
  ScopedUmask& operator=(const ScopedUmask&) = delete;

 private:
  mode_t previous_;
};

class ModelStreamerSessionTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    state.hold_end = false;
    state.hold_end_after_submission = 0;
    state.hold_start = false;
    state.hold_request = false;
    state.allow_responses = true;
    state.pause_after_completion = false;
    state.pause_after_response = false;
    state.completed_submissions = 0;
    state.allocation_calls = state.hold_allocation = state.allocation_entered = 0;
    state.fail_allocation = state.fail_mapping = 0;
    state.hold_producer = state.producer_at_window = false;
    state.producer_window_slots = 2;
    state.hold_producer_with_pending_front = false;
    state.arm_queue_failure = false;
    state.queue_allocation_bytes = 0;
    state.arm_slot_failure = state.fail_registration = false;
    state.queue_growth_failed = false;
    state.allocation_error = 0;
    state.preparation_cancellation = Cancellation{};
    state.cancel_after_directory.clear();
    state.cancel_after_permissions.clear();
    state.cancel_after_allocation = false;
    state.mappings = 0;
    state.allocated_descriptor = -1;
    state.permission_paths.clear();
    state.permission_calls = state.fail_permission_call = 0;
    state.start_status = state.credential_status = 0;
    state.request_status = RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
    state.return_submission_id = true;
    state.storage_failure_submission = 0;
    state.return_start_handle = true;
    state.sparse_allocations = false;
    state.starts = state.ends_entered = state.ends = state.submitted = 0;
    state.max_active_submissions = state.max_active_bytes = 0;
    state.allocations = state.requests_entered = 0;
    state.reject_request = 0;
    state.response_polls = 0;
    state.sources.clear();
    state.submission_sources.clear();
    state.submission_bytes.clear();
    state.fault = Fault::NONE;
    state.native_thread = {};
    state.next_id = 0;
    root_ = temporary_.path();
  }

  CheckpointInventory Inventory(unsigned files = 1) const
  {
    CheckpointInventory inventory;
    for (unsigned i = 0; i < files; ++i)
      inventory.files.push_back(CheckpointFile{"s3://test-bucket/object-" + std::to_string(i), std::to_string(i), 1});
    return inventory;
  }

  // Bounded waits make a broken drain fail without hanging the suite. Callers
  // always release the fake end barrier before destroying asynchronous work.
  bool WaitForEnd()
  {
    std::unique_lock lock(state.mutex);
    return state.changed.wait_for(lock, 5s, [] { return state.ends_entered != 0; });
  }
  void ReleaseEnd()
  {
    std::lock_guard lock(state.mutex);
    state.hold_end = false;
    state.changed.notify_all();
  }
  bool WaitForStopped()
  {
    std::unique_lock lock(state.mutex);
    return state.changed.wait_for(lock, 5s, [] { return state.ends == state.starts; });
  }
  bool WaitForSubmissions(unsigned count)
  {
    std::unique_lock lock(state.mutex);
    return state.changed.wait_for(lock, 5s, [count] { return state.submitted >= count; });
  }
  void ExpectCancellation(std::future<void>& result, CancellationError::Reason reason)
  {
    try {
      result.get();
      ADD_FAILURE() << "restore completed without cancellation";
    }
    catch (const CancellationError& error) {
      EXPECT_EQ(error.reason, reason);
    }
  }
  test::TemporaryDirectory temporary_;
  Path root_;
};
}  // namespace

extern "C" int __real_posix_fallocate(int descriptor, off_t offset, off_t length);
extern "C" Cancellation::Clock::time_point __real__ZNSt6chrono3_V212steady_clock3nowEv();
extern "C" void* __real_mmap(void* address, size_t length, int protection, int flags, int descriptor, off_t offset);
extern "C" bool __real__ZNSt10filesystem16create_directoryERKNS_7__cxx114pathE(const Path& path);
extern "C" void __real__ZNSt10filesystem11permissionsERKNS_7__cxx114pathENS_5permsENS_12perm_optionsE(
    const Path& path, fs::perms permissions, fs::perm_options options);

extern "C" Cancellation::Clock::time_point __wrap__ZNSt6chrono3_V212steady_clock3nowEv()
{
  if (auto* restore = producer_restore) {
    // Disable recursion from condition-variable deadline calculations.
    producer_restore = nullptr;
    {
      std::unique_lock lock(state.mutex);
      if (state.arm_slot_failure && state.mappings == 1 &&
          (state.submitted == 1 || ModelStreamerRestoreTestPeer::PendingCount(*restore) != 0)) {
        state.changed.wait(lock, [] { return state.submitted == 1; });
        state.arm_slot_failure = false;
        fail_next_allocation = true;
      }
      if (state.arm_queue_failure && state.mappings == 2 && state.permission_calls >= 2) {
        state.arm_queue_failure = false;
        // Skip path bookkeeping allocations; fail the measured pending-node growth.
        fail_pending_allocation_size = state.queue_allocation_bytes;
        state.changed.notify_all();
      }
      if (state.hold_producer && state.mappings == state.producer_window_slots &&
          ((state.submitted == 1 && ModelStreamerRestoreTestPeer::HasOnlyUnclaimedPending(*restore, state.producer_window_slots - 1)) ||
           (state.hold_producer_with_pending_front && state.requests_entered == 1 &&
            ModelStreamerRestoreTestPeer::PendingCount(*restore) == state.producer_window_slots))) {
        state.producer_at_window = true;
        state.changed.notify_all();
        state.changed.wait(lock, [] { return !state.hold_producer; });
      }
    }
    producer_restore = restore;
  }
  return __real__ZNSt6chrono3_V212steady_clock3nowEv();
}

// Wrap the filesystem entry points called by the restore code, since their
// underlying mkdir/chmod calls live inside the shared C++ standard library.
extern "C" bool __wrap__ZNSt10filesystem16create_directoryERKNS_7__cxx114pathE(const Path& path)
{
  const bool created = __real__ZNSt10filesystem16create_directoryERKNS_7__cxx114pathE(path);
  std::lock_guard lock(state.mutex);
  if (path == state.cancel_after_directory)
    state.preparation_cancellation.Cancel();
  return created;
}

extern "C" void __wrap__ZNSt10filesystem11permissionsERKNS_7__cxx114pathENS_5permsENS_12perm_optionsE(
    const Path& path, fs::perms permissions, fs::perm_options options)
{
  {
    std::lock_guard lock(state.mutex);
    if (++state.permission_calls == state.fail_permission_call)
      throw fs::filesystem_error("injected permission failure", path, std::make_error_code(std::errc::permission_denied));
  }
  __real__ZNSt10filesystem11permissionsERKNS_7__cxx114pathENS_5permsENS_12perm_optionsE(path, permissions, options);
  std::lock_guard lock(state.mutex);
  state.permission_paths.push_back(path);
  if (path == state.cancel_after_permissions)
    state.preparation_cancellation.Cancel();
}

extern "C" void* __wrap_mmap(void* address, size_t length, int protection, int flags, int descriptor, off_t offset)
{
  {
    std::lock_guard lock(state.mutex);
    ++state.mappings;
    if (state.mappings == state.fail_mapping) {
      errno = ENOMEM;
      return MAP_FAILED;
    }
  }
  return __real_mmap(address, length, protection, flags, descriptor, offset);
}

extern "C" int __wrap_posix_fallocate(int descriptor, off_t offset, off_t length)
{
  {
    std::unique_lock lock(state.mutex);
    const auto call = ++state.allocation_calls;
    if (call == state.hold_allocation) {
      state.allocation_entered = call;
      state.changed.notify_all();
      state.changed.wait(lock, [call] { return state.hold_allocation != call; });
    }
    if (call == state.fail_allocation)
      return ENOSPC;
    if (state.allocation_error != 0) {
      errno = EIO; // posix_fallocate returns its error directly, independently of errno.
      return state.allocation_error;
    }
    // Admission tests need large logical files without allocating gigabytes.
    if (state.sparse_allocations)
      return ftruncate(descriptor, offset + length) == 0 ? 0 : errno;
  }
  const int result = __real_posix_fallocate(descriptor, offset, length);
  if (result == 0) {
    std::lock_guard lock(state.mutex);
    ++state.allocations;
    state.allocated_descriptor = descriptor;
    if (state.cancel_after_allocation)
      state.preparation_cancellation.Cancel();
    state.changed.notify_all();
  }
  return result;
}

extern "C" int runai_file_streamer_start(void** value)
{
  std::unique_lock lock(state.mutex);
  state.native_thread = std::this_thread::get_id();
  *value = state.return_start_handle ? new Session : nullptr;
  ++state.starts;
  state.changed.notify_all();
  state.changed.wait(lock, [] { return !state.hold_start; });
  return state.start_status;
}

extern "C" int runai_file_streamer_set_credentials(void*, const char**, const char**, unsigned)
{
  std::lock_guard lock(state.mutex);
  EXPECT_EQ(state.native_thread, std::this_thread::get_id());
  return state.credential_status;
}

extern "C" int runai_file_streamer_set_fs_strategy(void*, const char*)
{
  return RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
}

extern "C" void runai_file_streamer_end(void* value)
{
  auto* session = static_cast<Session*>(value);
  {
    std::unique_lock lock(state.mutex);
    EXPECT_EQ(state.native_thread, std::this_thread::get_id());
    ++state.ends_entered;
    state.changed.notify_all();
    state.changed.wait(lock, [] {
      return !state.hold_end || state.submitted < state.hold_end_after_submission;
    });
    // Only wholly successful submissions release their pointers early. Failed
    // and unfinished entries can still be written during native teardown.
    for (auto& [id, entry] : session->entries) {
      for (std::size_t i = 0; i < entry.destinations.size(); ++i) {
        EXPECT_EQ(std::string(*entry.paths[i]), entry.sources[i]);
        EXPECT_GT(*entry.sizes[i], 0U);
        EXPECT_EQ(*entry.destination_slots[i], entry.destinations[i]);
        *entry.destinations[i] = 'e';
      }
    }
    ++state.ends;
    state.changed.notify_all();
  }
  delete session;
}

extern "C" int runai_file_streamer_request(
    void* value, RunaiFileStreamerSubmissionId* id, unsigned files, const char** paths,
    unsigned* ranges, std::size_t* offsets, std::size_t* sizes, void** destinations,
    RunaiFileStreamerDevice device)
{
  EXPECT_EQ(device.type, RUNAI_FILE_STREAMER_DEVICE_CPU);
  EXPECT_EQ(device.id, 0);
  auto& session = *static_cast<Session*>(value);
  std::unique_lock lock(state.mutex);
  EXPECT_EQ(state.native_thread, std::this_thread::get_id());
  ++state.requests_entered;
  state.changed.notify_all();
  state.changed.wait(lock, [] { return !state.hold_request; });
  if (state.requests_entered == state.reject_request) {
    *id = 0;
    return RUNAI_FILE_STREAMER_RESPONSE_FILE_ACCESS_ERROR;
  }
  if (state.request_status != RUNAI_FILE_STREAMER_RESPONSE_SUCCESS &&
      state.request_status != RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR && !state.return_submission_id) {
    *id = 0;
    return state.request_status;
  }
  const auto native_id = ++state.next_id;
  *id = state.return_submission_id ? native_id : 0;
  ++state.submitted;
  state.submission_sources.emplace_back();
  state.submission_bytes.push_back(0);
  for (unsigned file = 0; file < files; ++file) {
    state.sources.emplace_back(paths[file]);
    state.submission_sources.back().emplace_back(paths[file]);
    state.submission_bytes.back() += sizes[file];
    EXPECT_EQ(ranges[file], 1U);
    EXPECT_EQ(offsets[file], 0U);
    EXPECT_GT(sizes[file], 0U);
    auto* destination = static_cast<char*>(destinations[file]);
    *destination = 'd';
    session.entries[native_id].destinations.push_back(destination);
    session.entries[native_id].paths.push_back(&paths[file]);
    session.entries[native_id].sizes.push_back(&sizes[file]);
    session.entries[native_id].destination_slots.push_back(&destinations[file]);
    session.entries[native_id].sources.emplace_back(paths[file]);
    session.entries[native_id].bytes += sizes[file];
    session.responses.push_back(Response{native_id, file, file + 1 == files ? 1 : 0});
  }
  state.max_active_submissions = std::max(state.max_active_submissions, session.entries.size());
  uintmax_t active_bytes = 0;
  for (const auto& [entry_id, entry] : session.entries)
    active_bytes += entry.bytes;
  state.max_active_bytes = std::max(state.max_active_bytes, active_bytes);
  state.changed.notify_all();
  if (state.fail_registration) {
    state.fail_registration = false;
    fail_next_allocation = true;
  }
  return state.request_status;
}

extern "C" int runai_file_streamer_response(
    void* value, RunaiFileStreamerSubmissionId* id, unsigned* file, unsigned* range, int* done, unsigned)
{
  auto& session = *static_cast<Session*>(value);
  std::unique_lock lock(state.mutex);
  EXPECT_EQ(state.native_thread, std::this_thread::get_id());
  ++state.response_polls;
  state.changed.wait_for(lock, 1ms, [] { return state.allow_responses; });
  if (!state.allow_responses || state.fault == Fault::TIMEOUT)
    return RUNAI_FILE_STREAMER_RESPONSE_TIMED_OUT;
  if (session.next == session.responses.size())
    return RUNAI_FILE_STREAMER_RESPONSE_TIMED_OUT;
  const auto response = session.responses[session.next++];
  *id = response.id;
  *file = state.fault == Fault::INVALID_INDEX ? 999 : response.file;
  *range = 0;
  *done = response.done;
  if (response.done) {
    ++state.completed_submissions;
    if (state.pause_after_completion) {
      state.allow_responses = false;
      state.pause_after_completion = false;
    }
    state.changed.notify_all();
  }
  if (state.pause_after_response) {
    state.allow_responses = false;
    state.pause_after_response = false;
    state.changed.notify_all();
  }
  if (state.fault == Fault::EARLY_DONE)
    *done = 1;
  if (state.fault == Fault::MISSING_DONE)
    *done = 0;
  if (state.fault == Fault::UNKNOWN)
    return RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR;
  if (state.fault == Fault::STORAGE || response.id == state.storage_failure_submission) {
    session.entries[response.id].failed = true;
    return RUNAI_FILE_STREAMER_RESPONSE_FILE_ACCESS_ERROR;
  }
  if (state.fault == Fault::NONE && response.done && !session.entries[response.id].failed)
    session.entries.erase(response.id);
  return RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
}

extern "C" const char* runai_file_streamer_response_str(int) { return "injected native status"; }

TEST_F(ModelStreamerSessionTest, UnusedCoordinatorDoesNotStartWorker)
{
  {
    ModelStreamerRestore restore;
    EXPECT_FALSE(ModelStreamerRestoreTestPeer::HasWorker(restore));
  }
  EXPECT_EQ(state.starts, 0U);
  EXPECT_EQ(state.ends, 0U);
}

TEST_F(ModelStreamerSessionTest, CancellationDuringDirectoryCreationStopsBeforeStartingWorker)
{
  const auto destination = root_ / "cancelled";
  CheckpointInventory inventory;
  inventory.directories = {{"first"}, {"second"}};
  state.cancel_after_directory = destination / "first";
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, destination, state.preparation_cancellation), CancellationError);
  EXPECT_TRUE(fs::is_directory(destination / "first"));
  EXPECT_FALSE(fs::exists(destination / "second"));
  EXPECT_FALSE(ModelStreamerRestoreTestPeer::HasWorker(restore));
  EXPECT_EQ(state.starts, 0U);
}

TEST_F(ModelStreamerSessionTest, CancellationDuringLastDirectoryCreationStopsBeforeStartingWorker)
{
  const auto destination = root_ / "cancelled";
  CheckpointInventory inventory;
  inventory.directories = {{"last"}};
  state.cancel_after_directory = destination / "last";
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, destination, state.preparation_cancellation), CancellationError);
  EXPECT_TRUE(fs::is_directory(destination / "last"));
  EXPECT_FALSE(ModelStreamerRestoreTestPeer::HasWorker(restore));
  EXPECT_EQ(state.starts, 0U);
}

TEST_F(ModelStreamerSessionTest, CancellationDuringDirectoryPermissionsStopsRemainingUpdates)
{
  const auto destination = root_ / "cancelled";
  CheckpointInventory inventory;
  inventory.root_permissions = fs::perms::owner_all;
  inventory.directories = {{"parent", fs::perms::owner_all},
                           {"parent/child", fs::perms::owner_read | fs::perms::owner_exec},
                           {"last", fs::perms::owner_all}};
  state.cancel_after_permissions = destination / "parent/child";
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, destination, state.preparation_cancellation), CancellationError);
  EXPECT_EQ(state.permission_paths, (std::vector<Path>{destination / "last", destination / "parent/child"}));
  EXPECT_EQ(fs::status(destination / "parent/child").permissions(), fs::perms::owner_read | fs::perms::owner_exec);
  EXPECT_EQ(state.starts, 0U);
  EXPECT_FALSE(restore.Failed());
}

TEST_F(ModelStreamerSessionTest, CancellationDuringRootPermissionsIsReported)
{
  const auto destination = root_ / "cancelled";
  CheckpointInventory inventory;
  inventory.root_permissions = fs::perms::owner_all;
  state.cancel_after_permissions = destination;
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, destination, state.preparation_cancellation), CancellationError);
  EXPECT_EQ(state.permission_paths, (std::vector<Path>{destination}));
  EXPECT_EQ(state.starts, 0U);
  EXPECT_FALSE(restore.Failed());
}

TEST_F(ModelStreamerSessionTest, CancellationDuringAllocationClosesFileWithoutMappingOrSubmitting)
{
  const auto destination = root_ / "cancelled";
  state.cancel_after_allocation = true;
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(Inventory(2), destination, state.preparation_cancellation), CancellationError);
  EXPECT_EQ(state.allocations, 1U);
  EXPECT_EQ(state.mappings, 0U);
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_EQ(state.starts, 0U);
  EXPECT_FALSE(fs::exists(destination / "1"));
  EXPECT_FALSE(HasMapping(destination / "0"));
  ASSERT_GE(state.allocated_descriptor, 0);
  errno = 0;
  EXPECT_EQ(fcntl(state.allocated_descriptor, F_GETFD), -1);
  EXPECT_EQ(errno, EBADF);
  EXPECT_FALSE(restore.Failed());
}

TEST_F(ModelStreamerSessionTest, ReusesIdleSessionAndWorkerUntilShutdown)
{
  {
    ModelStreamerRestore restore;
    std::thread::id worker;
    for (unsigned i = 0; i < 20; ++i) {
      ASSERT_NO_THROW(restore.Stage(Inventory(), root_ / std::to_string(i)));
      EXPECT_TRUE(ModelStreamerRestoreTestPeer::HasWorker(restore));
      EXPECT_FALSE(HasMapping(root_ / std::to_string(i)));
      std::lock_guard lock(state.mutex);
      if (i == 0)
        worker = state.native_thread;
      EXPECT_EQ(state.native_thread, worker);
      EXPECT_EQ(state.starts, 1U);
      EXPECT_EQ(state.ends, 0U);
    }
    // A retained handle must not make the idle worker poll native responses.
    {
      std::unique_lock lock(state.mutex);
      const auto polls = state.response_polls;
      EXPECT_FALSE(state.changed.wait_for(lock, 50ms, [polls] { return state.response_polls != polls; }));
    }
    ModelStreamerRestoreTestPeer::RequestStop(restore);
    ModelStreamerRestoreTestPeer::RequestStop(restore);
    EXPECT_TRUE(WaitForStopped());
    EXPECT_THROW(restore.Stage(Inventory(), root_ / "stopped"), std::runtime_error);
    EXPECT_FALSE(restore.Failed());
  }
  EXPECT_EQ(state.ends, 1U);
}

TEST_F(ModelStreamerSessionTest, ShutdownDuringStartupRejectsPendingWithoutSubmitting)
{
  state.hold_start = true;
  ModelStreamerRestore restore;
  auto first = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "first"); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.starts == 1; }));
  }
  auto queued = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "queued"); });
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::WaitForPending(restore, 2));
  auto stop = std::async(std::launch::async, [&] { ModelStreamerRestoreTestPeer::RequestStop(restore); });
  const bool stopped = stop.wait_for(2s) == std::future_status::ready;
  EXPECT_TRUE(stopped);
  if (stopped) {
    EXPECT_THROW(restore.Stage(Inventory(), root_ / "late"), std::runtime_error);
  }
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 0U);
    state.hold_start = false;
    state.changed.notify_all();
  }
  stop.get();
  EXPECT_THROW(first.get(), std::runtime_error);
  EXPECT_THROW(queued.get(), std::runtime_error);
  EXPECT_FALSE(HasMapping(root_ / "first"));
  EXPECT_FALSE(HasMapping(root_ / "queued"));
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_EQ(state.ends, 1U);
  EXPECT_FALSE(restore.Failed());
}

TEST_F(ModelStreamerSessionTest, ShutdownDrainsClaimedBuffersAndRejectsNewWork)
{
  state.allow_responses = false;
  state.hold_end = true;
  ModelStreamerRestore restore;
  auto active = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "active"); });
  EXPECT_TRUE(WaitForSubmissions(1));
  ModelStreamerRestoreTestPeer::RequestStop(restore);
  const bool ending = WaitForEnd();
  EXPECT_TRUE(ending);
  EXPECT_EQ(active.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "active"));
  EXPECT_FALSE(restore.Failed());
  if (ending) {
    EXPECT_THROW(restore.Stage(Inventory(), root_ / "late"), std::runtime_error);
  }
  ReleaseEnd();
  EXPECT_THROW(active.get(), std::runtime_error);
  EXPECT_FALSE(HasMapping(root_ / "active"));
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_EQ(state.ends, 1U);
  EXPECT_FALSE(restore.Failed());
}

TEST_F(ModelStreamerSessionTest, AllocationFailureDoesNotSubmitNativeWritesAndAllowsRetry)
{
  state.allocation_error = ENOSPC;
  ModelStreamerRestore restore;
  try {
    restore.Stage(Inventory(), root_ / "full");
    FAIL() << "restore accepted a destination without backing storage";
  }
  catch (const std::system_error& error) {
    EXPECT_EQ(error.code(), std::errc::no_space_on_device);
  }
  EXPECT_EQ(state.starts, 0U);
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_FALSE(restore.Failed());

  state.allocation_error = 0;
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "retry"));
  EXPECT_EQ(state.submitted, 1U);
}

TEST_F(ModelStreamerSessionTest, SetsFilePermissionsBeforeSubmissionRegardlessOfUmask)
{
  ScopedUmask mask(0077);
  state.hold_request = true;
  ModelStreamerRestore restore;
  const std::vector<fs::perms> modes{
      fs::perms::none, fs::perms::owner_read,
      fs::perms::owner_read | fs::perms::group_read | fs::perms::others_read,
      fs::perms::owner_all | fs::perms::group_read | fs::perms::others_read,
      fs::perms::unknown};
  auto inventory = Inventory(2 * modes.size());
  for (std::size_t i = 0; i < modes.size(); ++i) {
    inventory.files[2 * i].permissions = modes[i];
    inventory.files[2 * i + 1].permissions = modes[i];
    inventory.files[2 * i + 1].size_bytes = 0;
  }
  auto result = std::async(std::launch::async, [&] { restore.Stage(inventory, root_ / "permissions"); });
  {
    std::unique_lock lock(state.mutex);
    const bool submitting = state.changed.wait_for(lock, 5s, [] { return state.requests_entered == 1; });
    EXPECT_TRUE(submitting);
    if (submitting) {
      // The fake has not written any data yet. Checking only after Stage would
      // also pass with the old second permission loop.
      EXPECT_EQ(state.submitted, 0U);
      for (const auto& file : inventory.files) {
        const auto expected = file.permissions == fs::perms::unknown ?
            fs::perms::owner_read | fs::perms::owner_write : file.permissions;
        EXPECT_EQ(fs::status(root_ / "permissions" / file.relative_path).permissions(), expected);
      }
      EXPECT_EQ(state.permission_calls, 8U);  // Unknown modes leave creation permissions intact.
    }
    state.hold_request = false;
    state.changed.notify_all();
  }
  ASSERT_NO_THROW(result.get());
  EXPECT_FALSE(HasMapping(root_ / "permissions"));
  for (const auto& file : inventory.files) {
    const auto path = root_ / "permissions" / file.relative_path;
    const auto expected = file.permissions == fs::perms::unknown ?
        fs::perms::owner_read | fs::perms::owner_write : file.permissions;
    EXPECT_EQ(fs::status(path).permissions(), expected);
    if (file.size_bytes != 0) {
      // Grant test-only read access after checking the final mode, including 0000.
      fs::permissions(path, fs::perms::owner_read, fs::perm_options::add);
      char data = 0;
      std::ifstream(path).get(data);
      EXPECT_EQ(data, 'd');
    }
  }
}

class FilePermissionFailureTest : public ModelStreamerSessionTest,
                                  public ::testing::WithParamInterface<bool> {};

TEST_P(FilePermissionFailureTest, ReleasesBatchMappingsWithoutSubmittingAndAllowsRetry)
{
  auto inventory = Inventory(2);
  for (auto& file : inventory.files)
    file.permissions = fs::perms::owner_read;
  if (GetParam())
    inventory.files.back().size_bytes = 0;
  state.fail_permission_call = 2;
  ModelStreamerRestore restore;
  try {
    restore.Stage(inventory, root_ / "failed");
    FAIL() << "restore ignored the permission-setting failure";
  }
  catch (const std::system_error& error) {
    EXPECT_EQ(error.code(), std::errc::permission_denied);
  }
  EXPECT_EQ(state.permission_calls, 2U);
  EXPECT_EQ(state.starts, 0U);
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_FALSE(HasMapping(root_ / "failed"));
  EXPECT_FALSE(restore.Failed());
  state.fail_permission_call = 0;
  EXPECT_NO_THROW(restore.Stage(inventory, root_ / "retry"));
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(HasMapping(root_ / "retry"));
}

TEST_P(FilePermissionFailureTest, CancellationDuringFilePermissionsReleasesMappingsWithoutSubmitting)
{
  auto inventory = Inventory(2);
  inventory.files.front().permissions = fs::perms::owner_read;
  if (GetParam())
    inventory.files.front().size_bytes = 0;
  const auto destination = root_ / "cancelled";
  state.cancel_after_permissions = destination / inventory.files.front().relative_path;
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, destination, state.preparation_cancellation), CancellationError);
  EXPECT_EQ(state.permission_calls, 1U);
  EXPECT_FALSE(fs::exists(destination / inventory.files.back().relative_path));
  EXPECT_FALSE(HasMapping(destination));
  EXPECT_EQ(state.starts, 0U);
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_FALSE(restore.Failed());
}

INSTANTIATE_TEST_SUITE_P(FileKinds, FilePermissionFailureTest, ::testing::Bool(),
    [](const ::testing::TestParamInfo<bool>& info) { return info.param ? "Empty" : "Mapped"; });

TEST_F(ModelStreamerSessionTest, RestoresManyFilesWithStableSourcesAndPermissions)
{
  auto inventory = Inventory(64);
  inventory.files[17].size_bytes = 0;
  std::vector<std::string> expected_sources;
  for (auto& file : inventory.files) {
    file.permissions = fs::perms::owner_read;
    if (file.size_bytes != 0)
      expected_sources.push_back(file.source_locator);
  }
  ModelStreamerRestore restore;
  ASSERT_NO_THROW(restore.Stage(inventory, root_ / "many"));
  EXPECT_EQ(state.sources, expected_sources);
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(HasMapping(root_ / "many"));
  for (const auto& file : inventory.files) {
    const Path staged = root_ / "many" / file.relative_path;
    EXPECT_EQ(fs::file_size(staged), file.size_bytes);
    EXPECT_EQ(fs::status(staged).permissions(), file.permissions);
    if (file.size_bytes != 0) {
      char data = 0;
      std::ifstream(staged).get(data);
      EXPECT_EQ(data, 'd');
    }
  }
}

TEST_F(ModelStreamerSessionTest, SplitsOneInventoryAtByteBudgetWithEmptyFiles)
{
  state.sparse_allocations = true;
  auto inventory = Inventory(6);
  const std::vector<uintmax_t> sizes{0, 4'000'000'000, 0, 6'000'000'000, 1, 0};
  for (size_t i = 0; i < inventory.files.size(); ++i) {
    inventory.files[i].size_bytes = sizes[i];
    inventory.files[i].permissions = fs::perms::owner_read;
  }
  ModelStreamerRestore restore;
  ASSERT_NO_THROW(restore.Stage(inventory, root_ / "split"));
  EXPECT_EQ(state.submission_bytes, (std::vector<uintmax_t>{10'000'000'000, 1}));
  EXPECT_EQ(state.submission_sources, (std::vector<std::vector<std::string>>{
      {inventory.files[1].source_locator, inventory.files[3].source_locator}, {inventory.files[4].source_locator}}));
  EXPECT_FALSE(HasMapping(root_ / "split"));
  for (const auto& file : inventory.files) {
    const Path staged = root_ / "split" / file.relative_path;
    EXPECT_EQ(fs::file_size(staged), file.size_bytes);
    EXPECT_EQ(fs::status(staged).permissions(), file.permissions);
    if (file.size_bytes) {
      char byte = 0;
      std::ifstream(staged).get(byte);
      EXPECT_EQ(byte, 'd');
    }
  }
}

TEST_F(ModelStreamerSessionTest, ConfiguredBudgetChangesBatchBoundaries)
{
  auto inventory = Inventory(5);
  const std::vector<uintmax_t> sizes{0, 4, 6, 1, 0};
  for (size_t i = 0; i < sizes.size(); ++i)
    inventory.files[i].size_bytes = sizes[i];
  {
    ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(10));
    ASSERT_NO_THROW(restore.Stage(inventory, root_ / "small-budget"));
    EXPECT_EQ(state.submission_bytes, (std::vector<uintmax_t>{10, 1}));
  }
  state.submission_bytes.clear();
  {
    ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(11));
    ASSERT_NO_THROW(restore.Stage(inventory, root_ / "large-budget"));
    EXPECT_EQ(state.submission_bytes, (std::vector<uintmax_t>{11}));
  }
  EXPECT_FALSE(HasMapping(root_));
  EXPECT_THROW((ModelStreamerRestore{{}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(0)}),
               std::invalid_argument);
}

TEST_F(ModelStreamerSessionTest, SubmitsOversizedFileAloneAndContinuesInventory)
{
  state.sparse_allocations = true;
  auto inventory = Inventory(2);
  inventory.files[0].size_bytes = 10'000'000'001;
  ModelStreamerRestore restore;
  ASSERT_NO_THROW(restore.Stage(inventory, root_ / "oversized"));
  EXPECT_EQ(state.submission_bytes, (std::vector<uintmax_t>{10'000'000'001, 1}));
  EXPECT_EQ(state.submission_sources, (std::vector<std::vector<std::string>>{
      {inventory.files[0].source_locator}, {inventory.files[1].source_locator}}));
  EXPECT_FALSE(HasMapping(root_ / "oversized"));
  for (const auto& file : inventory.files) {
    const Path staged = root_ / "oversized" / file.relative_path;
    EXPECT_EQ(fs::file_size(staged), file.size_bytes);
    char byte = 0;
    std::ifstream(staged).get(byte);
    EXPECT_EQ(byte, 'd');
  }
}

TEST_F(ModelStreamerSessionTest, OversizedSubmissionExcludesOtherReadsUntilCompletion)
{
  state.sparse_allocations = true;
  state.allow_responses = false;
  state.pause_after_completion = true;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(64));
  auto inventory = Inventory();
  inventory.files.front().size_bytes = 65;
  auto large = std::async(std::launch::async, [&] { restore.Stage(inventory, root_ / "large"); });
  EXPECT_TRUE(WaitForSubmissions(1));
  auto small = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "small"); });
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::WaitForPending(restore, 1));
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 1U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForSubmissions(2));
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.max_active_submissions, 1U);
    EXPECT_EQ(state.starts, 1U);
    EXPECT_EQ(state.ends, 0U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_NO_THROW(large.get());
  EXPECT_NO_THROW(small.get());
}

TEST_F(ModelStreamerSessionTest, LaterBatchFailureReleasesMappingsAndStopsInventory)
{
  state.sparse_allocations = true;
  state.storage_failure_submission = 2;
  state.hold_end = true;
  state.hold_end_after_submission = 2;
  state.hold_allocation = 3;
  auto inventory = Inventory(4);
  for (auto& file : inventory.files)
    file.size_bytes = 6'000'000'000;
  // This scenario retires A before preparing C, using a two-slot window.
  ModelStreamerRestore restore(
      {}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(kDefaultRestoreBatchBytes, 2));
  auto result = std::async(std::launch::async, [&] { restore.Stage(inventory, root_ / "partial"); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.ends_entered == 1; }));
  }
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_FALSE(HasMapping(root_ / "partial/0"));
  EXPECT_TRUE(HasMapping(root_ / "partial/1"));
  // C may already be preparing; hold it until B's failed native session ends.
  EXPECT_FALSE(fs::exists(root_ / "partial/3"));
  auto worker_progress = ModelStreamerRestoreTestPeer::QueueCancelledBarrier(restore);
  ReleaseEnd();
  EXPECT_EQ(worker_progress.wait_for(5s), std::future_status::ready);
  EXPECT_THROW(worker_progress.get(), CancellationError);
  {
    std::lock_guard lock(state.mutex);
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  EXPECT_THROW(result.get(), std::runtime_error);
  EXPECT_FALSE(fs::exists(root_ / "partial/3"));
  EXPECT_EQ(state.submission_sources, (std::vector<std::vector<std::string>>{
      {inventory.files[0].source_locator}, {inventory.files[1].source_locator}}));
  EXPECT_FALSE(HasMapping(root_ / "partial"));
  EXPECT_FALSE(restore.Failed());
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "retry"));
}

TEST_F(ModelStreamerSessionTest, LaterFileCreationFailureReleasesEarlierMappings)
{
  auto inventory = Inventory(2);
  // Validation accepts this path, but the destination name exceeds NAME_MAX.
  // The first file has already been mapped when creating the second fails.
  inventory.files.back().relative_path = std::string(300, 'x');
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, root_ / "partial"), std::system_error);
  EXPECT_TRUE(fs::exists(root_ / "partial" / "0"));
  EXPECT_FALSE(HasMapping(root_ / "partial"));
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "retry"));
}

enum class StartupFault { ErrorWithHandle, ErrorWithoutHandle, MissingHandle, Configuration };
class ModelStreamerStartupFailureTest : public ModelStreamerSessionTest,
                                       public ::testing::WithParamInterface<StartupFault> {};

TEST_P(ModelStreamerStartupFailureTest, ReleasesHandleBeforeReportingFailure)
{
  const auto fault = GetParam();
  state.return_start_handle = fault == StartupFault::ErrorWithHandle || fault == StartupFault::Configuration;
  const unsigned expected_ends = state.return_start_handle ? 1 : 0;
  if (fault == StartupFault::ErrorWithHandle || fault == StartupFault::ErrorWithoutHandle)
    state.start_status = RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR;
  if (fault == StartupFault::Configuration)
    state.credential_status = RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR;
  ModelStreamerSessionOptions options;
  options.region = "test-region";
  {
    ModelStreamerRestore restore(options);
    EXPECT_THROW(restore.Stage(Inventory(), root_ / "failed"), std::runtime_error);
    EXPECT_TRUE(restore.Failed());
    EXPECT_EQ(state.ends, expected_ends);
    EXPECT_EQ(state.requests_entered, 0U);
    state.start_status = state.credential_status = 0;
    state.return_start_handle = true;
    // Keep the failed coordinator alive: native teardown must finish before
    // reporting failure, without depending on the coordinator destructor.
    ModelStreamerRestore replacement(options);
    EXPECT_NO_THROW(replacement.Stage(Inventory(), root_ / "retry",
        Cancellation(Cancellation::Clock::now() + 2s)));
    EXPECT_EQ(state.starts, 2U);
    EXPECT_EQ(state.submitted, 1U);
  }
  EXPECT_EQ(state.ends, expected_ends + 1);
}

INSTANTIATE_TEST_SUITE_P(StartupFaults, ModelStreamerStartupFailureTest,
    ::testing::Values(StartupFault::ErrorWithHandle, StartupFault::ErrorWithoutHandle,
        StartupFault::MissingHandle, StartupFault::Configuration),
    [](const ::testing::TestParamInfo<StartupFault>& info) {
      switch (info.param) {
        case StartupFault::ErrorWithHandle: return "ErrorWithHandle";
        case StartupFault::ErrorWithoutHandle: return "ErrorWithoutHandle";
        case StartupFault::MissingHandle: return "MissingHandle";
        case StartupFault::Configuration: return "Configuration";
      }
      return "Unknown";
    });

TEST_F(ModelStreamerSessionTest, SuccessfulSubmissionReleasesMappingsWhileSessionStaysOpen)
{
  {
    ModelStreamerRestore restore;
    ASSERT_NO_THROW(restore.Stage(Inventory(), root_ / "restore"));
    EXPECT_FALSE(HasMapping(root_ / "restore"));
    EXPECT_EQ(state.ends, 0U);
    EXPECT_FALSE(restore.Failed());
    char data = 0;
    std::ifstream(root_ / "restore" / "0").get(data);
    EXPECT_EQ(data, 'd');
  }
  EXPECT_EQ(state.ends, 1U);
}

TEST_F(ModelStreamerSessionTest, DrainsBeforeReleasingFailures)
{
  for (const auto fault : {Fault::UNKNOWN, Fault::INVALID_INDEX,
                            Fault::EARLY_DONE, Fault::MISSING_DONE, Fault::TIMEOUT, Fault::STORAGE}) {
    SCOPED_TRACE(static_cast<int>(fault));
    state.fault = fault;
    state.hold_end = true;
    state.ends_entered = 0;
    ModelStreamerRestore restore(30ms);
    auto result = std::async(std::launch::async, [&] {
      restore.Stage(Inventory(2), root_ / std::to_string(static_cast<int>(fault)));
    });
    const bool ending = WaitForEnd();
    EXPECT_TRUE(ending);
    EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
    ReleaseEnd();
    EXPECT_THROW(result.get(), std::runtime_error);
    EXPECT_EQ(restore.Failed(), fault != Fault::STORAGE);
  }
}

struct SubmissionFailure {
  const char* name;
  int status;
  bool has_id;
  bool terminal;
};
class ModelStreamerSubmissionFailureTest : public ModelStreamerSessionTest,
    public ::testing::WithParamInterface<SubmissionFailure> {};

TEST_P(ModelStreamerSubmissionFailureTest, PreservesOwnershipUntilSafeToReturn)
{
  const auto failure = GetParam();
  state.request_status = failure.status;
  state.return_submission_id = failure.has_id;
  state.hold_end = true;
  ModelStreamerRestore restore;
  auto result = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "failed"); });
  if (failure.terminal) {
    EXPECT_TRUE(WaitForEnd());
  }
  const auto status = result.wait_for(failure.terminal ? 0ms : 2s);
  EXPECT_EQ(status, failure.terminal ? std::future_status::timeout : std::future_status::ready);
  if (failure.terminal || status == std::future_status::ready) {
    EXPECT_EQ(HasMapping(root_ / "failed"), failure.terminal);
  }
  // Even terminal failures cannot advertise a replaceable coordinator during teardown.
  EXPECT_FALSE(restore.Failed());
  ReleaseEnd();
  EXPECT_THROW(result.get(), std::runtime_error);
  EXPECT_FALSE(HasMapping(root_ / "failed"));
  EXPECT_EQ(restore.Failed(), failure.terminal);
  EXPECT_EQ(state.response_polls, 0U);
  if (!failure.terminal) {
    {
      std::lock_guard lock(state.mutex);
      state.request_status = RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
      state.return_submission_id = true;
    }
    EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "retry"));
    EXPECT_EQ(state.starts, 1U);
    EXPECT_EQ(state.ends, 0U);
    EXPECT_EQ(state.submitted, 1U);
  }
}

INSTANTIATE_TEST_SUITE_P(SubmissionResults, ModelStreamerSubmissionFailureTest,
    ::testing::Values(
        SubmissionFailure{"KnownErrorWithoutId", RUNAI_FILE_STREAMER_RESPONSE_FILE_ACCESS_ERROR, false, false},
        SubmissionFailure{"UnknownErrorWithoutId", RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR, false, true},
        SubmissionFailure{"KnownErrorWithId", RUNAI_FILE_STREAMER_RESPONSE_FILE_ACCESS_ERROR, true, true},
        SubmissionFailure{"UnknownErrorWithId", RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR, true, true},
        SubmissionFailure{"SuccessWithoutId", RUNAI_FILE_STREAMER_RESPONSE_SUCCESS, false, true}),
    [](const ::testing::TestParamInfo<SubmissionFailure>& info) { return info.param.name; });

TEST_F(ModelStreamerSessionTest, StorageFailureAllowsSubsequentRestore)
{
  ModelStreamerRestore restore;
  state.fault = Fault::STORAGE;
  EXPECT_THROW(restore.Stage(Inventory(), root_ / "failed"), std::runtime_error);
  EXPECT_FALSE(restore.Failed());
  EXPECT_EQ(state.ends, 1U);
  state.fault = Fault::NONE;
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "success"));
  EXPECT_EQ(state.starts, 2U);
  EXPECT_EQ(state.ends, 1U);
}

TEST_F(ModelStreamerSessionTest, DuplicateSubmissionIdRetainsBothEntriesUntilTeardown)
{
  state.allow_responses = false;
  state.hold_end = true;
  ModelStreamerRestore restore;
  auto first = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "first"); });
  EXPECT_TRUE(WaitForSubmissions(1));
  {
    std::lock_guard lock(state.mutex);
    // The next request accepts its destinations but cannot be registered in
    // active_. The pending queue must retain that entry through native end().
    state.next_id = 0;
  }
  auto duplicate = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "duplicate"); });
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(first.wait_for(0ms), std::future_status::timeout);
  EXPECT_EQ(duplicate.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "first"));
  EXPECT_TRUE(HasMapping(root_ / "duplicate"));
  EXPECT_FALSE(restore.Failed());
  ReleaseEnd();
  EXPECT_THROW(first.get(), std::runtime_error);
  EXPECT_THROW(duplicate.get(), std::runtime_error);
  EXPECT_FALSE(HasMapping(root_ / "first"));
  EXPECT_FALSE(HasMapping(root_ / "duplicate"));
  EXPECT_TRUE(restore.Failed());
  EXPECT_EQ(state.submitted, 2U);
  EXPECT_EQ(state.ends, 1U);
}

TEST_F(ModelStreamerSessionTest, ReplenishesSubmissionCapacityWhileOtherReadsContinue)
{
  state.allow_responses = false;
  state.pause_after_completion = true;
  ModelStreamerRestore restore;
  std::vector<std::future<void>> results;
  for (unsigned i = 0; i < 20; ++i)
    results.push_back(std::async(std::launch::async, [&, i] { restore.Stage(Inventory(), root_ / std::to_string(i)); }));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted >= 8; }));
    EXPECT_EQ(state.submitted, 8U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForSubmissions(9));
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 9U);
    EXPECT_EQ(state.completed_submissions, 1U);
    EXPECT_EQ(state.starts, 1U);
    EXPECT_EQ(state.ends, 0U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  for (auto& result : results)
    EXPECT_NO_THROW(result.get());
  EXPECT_LE(state.max_active_submissions, 8U);
  EXPECT_EQ(state.submitted, 20U);
  EXPECT_EQ(state.starts, 1U);
  EXPECT_EQ(state.ends, 0U);
}

TEST_F(ModelStreamerSessionTest, SuccessfulSubmissionReturnsWhileAnotherContinues)
{
  state.allow_responses = false;
  state.pause_after_completion = true;
  ModelStreamerRestore restore;
  auto first = std::async(std::launch::async, [&] { restore.Stage(Inventory(2), root_ / "first"); });
  EXPECT_TRUE(WaitForSubmissions(1));
  auto second = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "second"); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted == 2; }));
    state.allow_responses = true;
    state.changed.notify_all();
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.completed_submissions == 1; }));
  }
  const bool returned = first.wait_for(2s) == std::future_status::ready;
  EXPECT_TRUE(returned);
  if (returned) {
    EXPECT_FALSE(HasMapping(root_ / "first"));
    EXPECT_TRUE(HasMapping(root_ / "second"));
  }
  // Success releases this caller without stopping admission or the other read.
  auto queued = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "queued");
  });
  EXPECT_TRUE(WaitForSubmissions(3));
  EXPECT_EQ(queued.wait_for(0ms), std::future_status::timeout);
  EXPECT_EQ(second.wait_for(0ms), std::future_status::timeout);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 3U);
    EXPECT_EQ(state.ends, 0U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_NO_THROW(queued.get());
  EXPECT_NO_THROW(first.get());
  EXPECT_NO_THROW(second.get());
  EXPECT_FALSE(restore.Failed());
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "same-session"));
  EXPECT_EQ(state.starts, 1U);
}

TEST_F(ModelStreamerSessionTest, FirstRangeFailureStopsAdmissionUntilSessionDrains)
{
  state.fault = Fault::STORAGE;
  state.allow_responses = false;
  state.pause_after_response = true;
  ModelStreamerRestore restore;
  auto first = std::async(std::launch::async, [&] { restore.Stage(Inventory(2), root_ / "first"); });
  EXPECT_TRUE(WaitForSubmissions(1));
  auto second = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "second"); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted == 2; }));
    state.allow_responses = true;
    state.changed.notify_all();
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return !state.pause_after_response; }));
  }
  // One range failed before either submission completed. A new
  // batch must remain pending, so its deadline cannot cancel those native reads.
  auto queued = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "queued", Cancellation(Cancellation::Clock::now() + 100ms));
  });
  EXPECT_EQ(queued.wait_for(2s), std::future_status::ready);
  EXPECT_EQ(first.wait_for(0ms), std::future_status::timeout);
  EXPECT_EQ(second.wait_for(0ms), std::future_status::timeout);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 2U);
    EXPECT_EQ(state.ends, 0U);
    state.fault = Fault::NONE;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  ExpectCancellation(queued, CancellationError::Reason::DEADLINE_EXCEEDED);
  EXPECT_THROW(first.get(), std::runtime_error);
  EXPECT_NO_THROW(second.get());
  EXPECT_FALSE(restore.Failed());
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "next-session"));
  EXPECT_EQ(state.starts, 2U);
}

TEST_F(ModelStreamerSessionTest, LaterFailureDoesNotRetainSuccessfulSubmission)
{
  for (const auto fault : {Fault::STORAGE, Fault::UNKNOWN}) {
    SCOPED_TRACE(static_cast<int>(fault));
    state.fault = Fault::NONE;
    state.allow_responses = false;
    state.pause_after_completion = true;
    state.hold_end = true;
    state.ends_entered = 0;
    const unsigned submitted = state.submitted;
    const Path directory = root_ / std::to_string(static_cast<int>(fault));
    fs::create_directory(directory);
    ModelStreamerRestore restore;
    auto first = std::async(std::launch::async, [&] { restore.Stage(Inventory(), directory / "first"); });
    EXPECT_TRUE(WaitForSubmissions(submitted + 1));
    auto second = std::async(std::launch::async, [&] { restore.Stage(Inventory(), directory / "second"); });
    EXPECT_TRUE(WaitForSubmissions(submitted + 2));
    {
      std::lock_guard lock(state.mutex);
      state.allow_responses = true;
      state.changed.notify_all();
    }
    const bool returned = first.wait_for(2s) == std::future_status::ready;
    EXPECT_TRUE(returned);
    if (returned) {
      EXPECT_FALSE(HasMapping(directory / "first"));
    }
    EXPECT_TRUE(HasMapping(directory / "second"));
    {
      std::lock_guard lock(state.mutex);
      state.fault = fault;
      state.allow_responses = true;
      state.changed.notify_all();
    }
    EXPECT_TRUE(WaitForEnd());
    EXPECT_EQ(second.wait_for(0ms), std::future_status::timeout);
    EXPECT_TRUE(HasMapping(directory / "second"));
    ReleaseEnd();
    EXPECT_NO_THROW(first.get());
    EXPECT_THROW(second.get(), std::runtime_error);
    EXPECT_FALSE(HasMapping(directory / "second"));
    EXPECT_EQ(restore.Failed(), fault == Fault::UNKNOWN);
  }
}

TEST_F(ModelStreamerSessionTest, ReplenishesByteCapacityWhileOtherReadsContinue)
{
  state.sparse_allocations = true;
  state.allow_responses = false;
  state.pause_after_completion = true;
  ModelStreamerRestore restore;
  auto inventory = Inventory();
  // Sparse files exercise admission without allocating or reading gigabytes.
  inventory.files.front().size_bytes = 4'000'000'000;
  std::vector<std::future<void>> results;
  for (unsigned i = 0; i < 3; ++i)
    results.push_back(std::async(std::launch::async, [&, i] { restore.Stage(inventory, root_ / std::to_string(i)); }));
  EXPECT_TRUE(WaitForSubmissions(2));
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::WaitForPending(restore, 1));
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 2U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForSubmissions(3));
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.completed_submissions, 1U);
    EXPECT_EQ(state.max_active_submissions, 2U);
    EXPECT_LE(state.max_active_bytes, 10'000'000'000U);
    EXPECT_EQ(state.starts, 1U);
    EXPECT_EQ(state.ends, 0U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  for (auto& result : results)
    EXPECT_NO_THROW(result.get());
}

TEST_F(ModelStreamerSessionTest, TerminalFailureDrainsActiveAndQueuedRestores)
{
  state.allow_responses = false;
  state.hold_end = true;
  state.fault = Fault::UNKNOWN;
  ModelStreamerRestore restore;
  std::vector<std::future<void>> results;
  for (unsigned i = 0; i < 12; ++i)
    results.push_back(std::async(std::launch::async, [&, i] { restore.Stage(Inventory(), root_ / std::to_string(i)); }));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted >= 8; }));
  }
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::WaitForPending(restore, 4));
  {
    std::lock_guard lock(state.mutex);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForEnd());
  EXPECT_FALSE(restore.Failed());
  EXPECT_THROW(restore.Stage(Inventory(), root_ / "late"), std::runtime_error);
  // Every accepted destination must stay alive until the shared session ends.
  const auto waiting = std::count_if(results.begin(), results.end(), [](auto& result) {
    return result.wait_for(0ms) == std::future_status::timeout;
  });
  EXPECT_GE(waiting, 8);
  ReleaseEnd();
  for (auto& result : results)
    EXPECT_THROW(result.get(), std::runtime_error);
  EXPECT_TRUE(restore.Failed());
  EXPECT_EQ(state.starts, 1U);
  EXPECT_EQ(state.ends, 1U);
}

class QueuedCancellationTest : public ModelStreamerSessionTest, public ::testing::WithParamInterface<bool> {};

TEST_P(QueuedCancellationTest, DoesNotWaitForOrStopActiveReads)
{
  const bool deadline = GetParam();
  state.sparse_allocations = true;
  state.allow_responses = false;
  ModelStreamerRestore restore;
  auto inventory = Inventory();
  inventory.files.front().size_bytes = 6'000'000'000;
  auto active = std::async(std::launch::async, [&] { restore.Stage(inventory, root_ / "active"); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted == 1; }));
  }
  Cancellation cancellation;
  if (deadline)
    cancellation = cancellation.WithDeadline(Cancellation::Clock::now() + 2s);
  auto queued = std::async(std::launch::async, [&] { restore.Stage(inventory, root_ / "queued", cancellation); });
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::WaitForPending(restore, 1));
  if (!deadline)
    cancellation.Cancel();
  const auto queued_status = queued.wait_for(5s);
  EXPECT_EQ(queued_status, std::future_status::ready);
  EXPECT_EQ(active.wait_for(0ms), std::future_status::timeout);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 1U);
    EXPECT_EQ(state.ends, 0U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  ExpectCancellation(queued, deadline ? CancellationError::Reason::DEADLINE_EXCEEDED :
                                      CancellationError::Reason::CANCELLED);
  EXPECT_NO_THROW(active.get());
  EXPECT_FALSE(restore.Failed());
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "success"));
}

INSTANTIATE_TEST_SUITE_P(CancellationCauses, QueuedCancellationTest, ::testing::Bool());

TEST_F(ModelStreamerSessionTest, SubmissionTimeoutDrainsBeforeReturning)
{
  state.allow_responses = false;
  state.hold_end = true;
  Cancellation cancellation;
  ModelStreamerRestore restore(50ms);
  auto result = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "restore", cancellation);
  });
  const bool timed_out = WaitForEnd();
  EXPECT_TRUE(timed_out);
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "restore" / "0"));
  // Also release a broken implementation that did not enforce the timeout.
  if (!timed_out)
    cancellation.Cancel();
  ReleaseEnd();
  try {
    result.get();
    ADD_FAILURE() << "restore completed without timing out";
  }
  catch (const std::runtime_error& error) {
    EXPECT_NE(std::string(error.what()).find("timed out"), std::string::npos);
  }
  EXPECT_FALSE(HasMapping(root_ / "restore" / "0"));
  EXPECT_TRUE(restore.Failed());
}

TEST_F(ModelStreamerSessionTest, ActiveCancellationWaitsForNativeTeardown)
{
  state.allow_responses = false;
  state.hold_end = true;
  Cancellation stop;
  ModelStreamerRestore restore;
  Cancellation cancellation;
  cancellation = stop;
  auto result = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "restore", cancellation); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted == 1; }));
  }
  stop.Cancel();
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  ReleaseEnd();
  EXPECT_THROW(result.get(), CancellationError);
  EXPECT_TRUE(restore.Failed());
}

class SharedSessionCancellationTest : public ModelStreamerSessionTest,
                                      public ::testing::WithParamInterface<bool> {};

TEST_P(SharedSessionCancellationTest, ReportsEachRequestsOwnCancellationAfterDrain)
{
  const bool deadline = GetParam();
  state.allow_responses = false;
  state.hold_end = true;
  Cancellation trigger;
  if (deadline)
    trigger = trigger.WithDeadline(Cancellation::Clock::now() + 1s);
  Cancellation cancelled_peer;
  ModelStreamerRestore restore;
  auto first = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "first", trigger); });
  auto live = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "live"); });
  auto cancelled = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "cancelled", cancelled_peer);
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted == 3; }));
  }
  if (!deadline)
    trigger.Cancel();
  const bool ending = WaitForEnd();
  EXPECT_TRUE(ending);
  EXPECT_EQ(first.wait_for(0ms), std::future_status::timeout);
  EXPECT_EQ(live.wait_for(0ms), std::future_status::timeout);
  EXPECT_EQ(cancelled.wait_for(0ms), std::future_status::timeout);
  cancelled_peer.Cancel();

  const auto expect_session_failure = [](auto&& operation) {
    try {
      operation();
      ADD_FAILURE() << "restore completed despite shared session failure";
    }
    catch (const CancellationError&) {
      ADD_FAILURE() << "restore inherited another request's cancellation";
    }
    catch (const std::runtime_error& error) {
      EXPECT_STREQ(error.what(), "Model Streamer session interrupted by another submission's cancellation");
    }
  };
  // Queue closure rejects new work with the retained exception while end
  // is blocked. It must apply the same per-request attribution as active work.
  if (ending)
    expect_session_failure([&] { restore.Stage(Inventory(), root_ / "late"); });
  ReleaseEnd();
  ExpectCancellation(first, deadline ? CancellationError::Reason::DEADLINE_EXCEEDED :
                                      CancellationError::Reason::CANCELLED);
  expect_session_failure([&] { live.get(); });
  ExpectCancellation(cancelled, CancellationError::Reason::CANCELLED);
  EXPECT_TRUE(restore.Failed());
  EXPECT_EQ(state.submitted, 3U);
  EXPECT_EQ(state.ends, 1U);
}

INSTANTIATE_TEST_SUITE_P(CancellationCauses, SharedSessionCancellationTest, ::testing::Bool(),
    [](const ::testing::TestParamInfo<bool>& info) { return info.param ? "Deadline" : "ExplicitStop"; });

class StartupCancellationTest : public ModelStreamerSessionTest, public ::testing::WithParamInterface<bool> {};

TEST_P(StartupCancellationTest, DoesNotFailCoordinator)
{
  const bool deadline = GetParam();
  state.hold_start = true;
  Cancellation stop;
  ModelStreamerRestore restore;
  Cancellation cancellation;
  cancellation = deadline ? stop.WithDeadline(Cancellation::Clock::now() + 2s) : stop;
  auto cancelled = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "cancelled", cancellation);
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.starts == 1; }));
  }
  if (!deadline)
    stop.Cancel();
  EXPECT_EQ(cancelled.wait_for(5s), std::future_status::ready);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 0U);
    state.hold_start = false;
    state.changed.notify_all();
  }
  ExpectCancellation(cancelled, deadline ? CancellationError::Reason::DEADLINE_EXCEEDED :
                                         CancellationError::Reason::CANCELLED);
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_FALSE(restore.Failed());
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "success"));
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_EQ(state.starts, 1U);
  EXPECT_EQ(state.ends, 0U);
}

INSTANTIATE_TEST_SUITE_P(CancellationCauses, StartupCancellationTest, ::testing::Bool());

// Exercise explicit cancellation and deadline expiry while end() blocks the worker.
class QueuedTeardownCancellationTest : public ModelStreamerSessionTest,
                                       public ::testing::WithParamInterface<bool> {};

TEST_P(QueuedTeardownCancellationTest, ReturnsBeforeTeardownAndNeverSubmitsCancelledBuffers)
{
  const bool deadline = GetParam();
  state.hold_end = true;
  state.fault = Fault::STORAGE;
  ModelStreamerRestore restore;
  auto active = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "active"); });
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(active.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "active"));

  Cancellation cancellation;
  if (deadline)
    cancellation = cancellation.WithDeadline(Cancellation::Clock::now() + 2s);
  auto queued = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "queued", cancellation); });
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::WaitForPending(restore, 1));
  if (!deadline)
    cancellation.Cancel();
  const auto queued_status = queued.wait_for(3s);
  EXPECT_EQ(queued_status, std::future_status::ready);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 1U);
    EXPECT_EQ(state.ends, 0U);
  }
  if (queued_status == std::future_status::ready) {
    EXPECT_FALSE(HasMapping(root_ / "queued"));
  }
  // A ready result means Stage has released its mappings. Remove staging before
  // allowing the worker to inspect the queued entry; its pointer targets are gone.
  if (queued_status == std::future_status::ready) {
    std::error_code error;
    fs::remove_all(root_ / "queued", error);
    EXPECT_FALSE(error);
  }
  ReleaseEnd();
  EXPECT_THROW(active.get(), std::runtime_error);
  EXPECT_FALSE(HasMapping(root_ / "active"));
  ExpectCancellation(queued, deadline ? CancellationError::Reason::DEADLINE_EXCEEDED :
                                       CancellationError::Reason::CANCELLED);
  {
    std::lock_guard lock(state.mutex);
    state.fault = Fault::NONE;
  }
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "recovered"));
  EXPECT_EQ(state.submitted, 2U);
  EXPECT_FALSE(restore.Failed());
}

INSTANTIATE_TEST_SUITE_P(CancellationCauses, QueuedTeardownCancellationTest, ::testing::Bool());

TEST_F(ModelStreamerSessionTest, CancellationDuringNativeRequestWaitsForTeardown)
{
  state.hold_request = true;
  state.hold_end = true;
  state.allow_responses = false;
  Cancellation cancellation;
  ModelStreamerRestore restore;
  auto result = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "restore", cancellation); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.requests_entered == 1; }));
    EXPECT_EQ(state.submitted, 0U);
  }
  cancellation.Cancel();
  EXPECT_EQ(result.wait_for(100ms), std::future_status::timeout);
  {
    std::lock_guard lock(state.mutex);
    state.hold_request = false;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  ReleaseEnd();
  ExpectCancellation(result, CancellationError::Reason::CANCELLED);
  char data = 0;
  std::ifstream(root_ / "restore" / "0").get(data);
  EXPECT_EQ(data, 'e');
  EXPECT_TRUE(restore.Failed());
}

struct PipelineBudget {
  std::uint64_t budget;
  uintmax_t file_bytes;
  const char* name;
  unsigned slots = 2;
};
class PipelineWindowTest : public ModelStreamerSessionTest,
                           public ::testing::WithParamInterface<PipelineBudget> {};

TEST_P(PipelineWindowTest, BoundsPreparationAndWrapsConfiguredWindow)
{
  const auto settings = GetParam();
  state.sparse_allocations = true;
  state.allow_responses = false;
  state.pause_after_completion = true;
  state.hold_producer = true;
  state.producer_window_slots = settings.slots;
  state.hold_allocation = settings.slots + 1;
  ModelStreamerRestore restore(
      {}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(settings.budget, settings.slots));
  const unsigned files = settings.slots * 3 + 1;
  auto inventory = Inventory(files);
  for (auto& file : inventory.files)
    file.size_bytes = settings.file_bytes;
  auto result = std::async(std::launch::async, [&] {
    producer_restore = &restore;
    restore.Stage(inventory, root_ / "pipeline");
    producer_restore = nullptr;
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.producer_at_window; }));
    EXPECT_EQ(state.submitted, 1U);
    EXPECT_EQ(state.allocation_calls, settings.slots);
    EXPECT_EQ(state.mappings, settings.slots);
    EXPECT_EQ(ModelStreamerRestoreTestPeer::PendingCount(restore), settings.slots - 1);
    EXPECT_FALSE(fs::exists(root_ / "pipeline" / std::to_string(settings.slots)));
    for (unsigned i = 0; i < settings.slots; ++i)
      EXPECT_TRUE(HasMapping(root_ / "pipeline" / std::to_string(i)));
    state.allow_responses = true;
    state.changed.notify_all();
    // With one slot there is no next batch; larger windows advance without the caller.
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [&] {
      return settings.slots == 1 ? state.completed_submissions == 1 : state.submitted == 2;
    }));
    EXPECT_EQ(state.submitted, settings.slots == 1 ? 1U : 2U);
    EXPECT_EQ(state.allocation_calls, settings.slots);
    EXPECT_EQ(state.max_active_submissions, 1U);
    EXPECT_EQ(state.max_active_bytes, settings.file_bytes);
    state.hold_producer = false;
    state.changed.notify_all();
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [&] { return state.allocation_entered == settings.slots + 1; }));
    EXPECT_FALSE(HasMapping(root_ / "pipeline/0"));
    if (settings.slots > 1) {
      EXPECT_TRUE(HasMapping(root_ / "pipeline/1"));
    }
    EXPECT_FALSE(HasMapping(root_ / "pipeline" / std::to_string(settings.slots)));
    state.hold_allocation = 0;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_NO_THROW(result.get());
  EXPECT_EQ(state.submission_bytes, std::vector<uintmax_t>(files, settings.file_bytes));
  for (unsigned i = 0; i < files; ++i) {
    EXPECT_EQ(state.submission_sources.at(i), std::vector<std::string>{inventory.files[i].source_locator});
    char data = 0;
    std::ifstream(root_ / "pipeline" / std::to_string(i)).get(data);
    EXPECT_EQ(data, 'd');
  }
  EXPECT_FALSE(HasMapping(root_ / "pipeline"));
  EXPECT_EQ(state.starts, 1U);
  EXPECT_EQ(state.ends, 0U);
}

INSTANTIATE_TEST_SUITE_P(Budgets, PipelineWindowTest,
    ::testing::Values(PipelineBudget{10, 10, "TenBytes"},
                      PipelineBudget{11, 11, "ElevenBytes"},
                      PipelineBudget{10, 10, "OneSlot", 1},
                      PipelineBudget{10, 10, "ThreeSlots", 3},
                      PipelineBudget{10, 10, "FourSlots", 4},
                      PipelineBudget{10, 10, "TwentySlots", 20},
                      PipelineBudget{kDefaultRestoreBatchBytes, 20'890'000'000, "Oversized", 4}),
    [](const ::testing::TestParamInfo<PipelineBudget>& info) { return info.param.name; });

class PipelineRecoverableFailureTest : public ModelStreamerSessionTest,
                                      public ::testing::WithParamInterface<std::tuple<bool, unsigned>> {};

TEST_P(PipelineRecoverableFailureTest, RejectsQueuedSiblingBeforeCallerResumesAndAllowsPeer)
{
  const auto [rejection, slots] = GetParam();
  state.allow_responses = false;
  state.hold_producer = true;
  state.producer_window_slots = slots;
  state.hold_producer_with_pending_front = rejection;
  state.hold_request = rejection;
  state.reject_request = rejection ? 1 : 0;
  state.storage_failure_submission = rejection ? 0 : 1;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(2, slots));
  auto inventory = Inventory(slots + 2);
  for (auto& file : inventory.files)
    file.size_bytes = 2;
  Cancellation parent;
  auto failed = std::async(std::launch::async, [&] {
    producer_restore = &restore;
    restore.Stage(inventory, root_ / "failed", parent);
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.producer_at_window; }));
  }
  auto peer = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "peer");
  });
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::WaitForPending(restore, rejection ? slots + 1 : slots));
  {
    std::lock_guard lock(state.mutex);
    state.hold_request = false;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  // The worker must reject the sibling and finish independent work even while
  // the failed restore's caller cannot observe its first batch's error.
  EXPECT_EQ(peer.wait_for(5s), std::future_status::ready);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, rejection ? 1U : 2U);
    EXPECT_EQ(state.requests_entered, 2U);
    EXPECT_TRUE(state.hold_producer);
    state.hold_producer = false;
    state.changed.notify_all();
  }
  EXPECT_NO_THROW(peer.get());
  try {
    failed.get();
    ADD_FAILURE() << "restore lost its original failure";
  }
  catch (const std::runtime_error& error) {
    EXPECT_EQ(std::string(error.what()), rejection ? "submit Model Streamer restore: injected native status" :
                                                   "Model Streamer restore range: injected native status");
  }
  EXPECT_FALSE(parent.IsCancelled());
  EXPECT_FALSE(restore.Failed());
  EXPECT_FALSE(HasMapping(root_));
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "retry"));
}

INSTANTIATE_TEST_SUITE_P(FailureCauses, PipelineRecoverableFailureTest,
    ::testing::Combine(::testing::Bool(), ::testing::Values(1U, 2U, 3U, 4U)),
    [](const auto& info) {
      return std::string(std::get<0>(info.param) ? "SubmissionRejected" : "StorageError") +
          "Slots" + std::to_string(std::get<1>(info.param));
    });

TEST_F(ModelStreamerSessionTest, FailedQueuedBatchDoesNotBlockPeerOnByteBudget)
{
  state.allow_responses = false;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(2));
  auto active = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "active");
  });
  EXPECT_TRUE(WaitForSubmissions(1));
  // This failed batch cannot fit beside the active byte. Remove it before
  // budget admission so a healthy byte-sized peer behind it can still start.
  auto failed = ModelStreamerRestoreTestPeer::QueueFailedBatch(restore, 3);
  auto peer = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "peer");
  });
  EXPECT_TRUE(WaitForSubmissions(2));
  EXPECT_EQ(failed.wait_for(0ms), std::future_status::ready);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.starts, 1U);
    EXPECT_EQ(state.ends, 0U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_THROW(failed.get(), std::runtime_error);
  EXPECT_NO_THROW(active.get());
  EXPECT_NO_THROW(peer.get());
  EXPECT_FALSE(restore.Failed());
}

enum class PreparationFault { Allocation, Mapping, Permissions };
class PipelinePreparationFailureTest : public ModelStreamerSessionTest,
                                      public ::testing::WithParamInterface<std::tuple<PreparationFault, unsigned>> {};

TEST_P(PipelinePreparationFailureTest, DrainsBorrowedPointersAndPreservesPrimaryError)
{
  const auto [fault, slots] = GetParam();
  state.allow_responses = false;
  state.hold_allocation = slots;
  state.hold_end = true;
  if (fault == PreparationFault::Allocation)
    state.fail_allocation = slots;
  else if (fault == PreparationFault::Mapping)
    state.fail_mapping = slots;
  else
    state.fail_permission_call = slots;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, slots));
  Cancellation parent;
  auto sibling = Cancellation::ChildOf(parent);
  auto result = std::async(std::launch::async, [&] {
    // Stage's borrowed paths die as this owning preparation unwinds.
    PreparedRestore preparation;
    preparation.inventory = Inventory(slots + 2);
    for (auto& file : preparation.inventory.files) {
      file.source_locator += std::string(80, 'x');
      file.permissions = fs::perms::owner_read;
    }
    restore.Stage(preparation.inventory, root_ / "failure", parent);
  });
  EXPECT_TRUE(WaitForSubmissions(1));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [&] { return state.allocation_entered == slots; }));
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForEnd());
  EXPECT_FALSE(parent.IsCancelled());
  EXPECT_FALSE(sibling.IsCancelled());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "failure/0"));
  EXPECT_FALSE(fs::exists(root_ / "failure" / std::to_string(slots)));
  // A later parent stop during drain must not replace the preparation error.
  parent.Cancel();
  ReleaseEnd();
  try {
    result.get();
    ADD_FAILURE() << "preparation failure was lost";
  }
  catch (const std::system_error& error) {
    const auto expected = fault == PreparationFault::Allocation ? std::errc::no_space_on_device :
                          fault == PreparationFault::Mapping ? std::errc::not_enough_memory :
                          std::errc::permission_denied;
    EXPECT_EQ(error.code(), expected);
  }
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(HasMapping(root_ / "failure"));
}

INSTANTIATE_TEST_SUITE_P(Faults, PipelinePreparationFailureTest,
    ::testing::Combine(::testing::Values(PreparationFault::Allocation, PreparationFault::Mapping, PreparationFault::Permissions),
                      ::testing::Values(2U, 4U)),
    [](const auto& info) {
      const char* name = std::get<0>(info.param) == PreparationFault::Allocation ? "Allocation" :
                         std::get<0>(info.param) == PreparationFault::Mapping ? "Mapping" : "Permissions";
      return std::string(name) + "Slots" + std::to_string(std::get<1>(info.param));
    });

TEST_F(ModelStreamerSessionTest, PipelineQueueAllocationFailureCompletesUnpublishedPromise)
{
  ModelStreamerRestore restore;
  ModelStreamerRestoreTestPeer::ExpectQueueAllocationFailure(restore);
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "retry"));
}

TEST_F(ModelStreamerSessionTest, PipelineQueuePublicationFailureDoesNotCancelClaimedPeer)
{
  state.allow_responses = false;
  state.hold_request = true;
  state.hold_allocation = 2;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1));
  auto active_inventory = Inventory();
  active_inventory.files[0].permissions = fs::perms::owner_read;
  auto failed_inventory = Inventory(4);
  for (auto& file : failed_inventory.files)
    file.permissions = fs::perms::owner_read;
  auto active = std::async(std::launch::async, [&] {
    restore.Stage(active_inventory, root_ / "active-peer");
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.requests_entered == 1; }));
  }
  // A owns native access. The failing caller's front has not been published.
  std::size_t actual_fault_bytes = 0;
  auto failed = std::async(std::launch::async, [&] {
    producer_restore = &restore;
    try {
      restore.Stage(failed_inventory, root_ / "queue-failure");
    }
    catch (const std::bad_alloc&) {
      actual_fault_bytes = last_failed_allocation_size;
      throw;
    }
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 2; }));
  }
  ModelStreamerRestoreTestPeer::ExpectQueueAllocationFailure(restore, true);
  const auto expected_fault_bytes = last_failed_allocation_size;
  EXPECT_GT(expected_fault_bytes, 0U);
  {
    std::lock_guard lock(state.mutex);
    state.queue_allocation_bytes = expected_fault_bytes;
    state.arm_queue_failure = true;
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  EXPECT_EQ(failed.wait_for(5s), std::future_status::ready);
  EXPECT_THROW(failed.get(), std::bad_alloc);
  EXPECT_EQ(actual_fault_bytes, expected_fault_bytes);
  EXPECT_FALSE(HasMapping(root_ / "queue-failure"));
  EXPECT_TRUE(HasMapping(root_ / "active-peer"));
  EXPECT_FALSE(fs::exists(root_ / "queue-failure/1"));
  {
    std::lock_guard lock(state.mutex);
    state.hold_request = false;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_NO_THROW(active.get());
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_EQ(state.ends, 0U);
  EXPECT_FALSE(restore.Failed());
}

class PipelineCapacityTest : public ModelStreamerSessionTest,
                             public ::testing::WithParamInterface<unsigned> {};

TEST_P(PipelineCapacityTest, WithdrawsAllPendingSiblingsBeforeActiveDrain)
{
  const auto slots = GetParam();
  state.allow_responses = false;
  state.hold_producer = true;
  state.producer_window_slots = slots;
  state.hold_end = true;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, slots));
  Cancellation stop;
  auto result = std::async(std::launch::async, [&] {
    producer_restore = &restore;
    restore.Stage(Inventory(slots + 2), root_ / "withdraw", stop);
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.producer_at_window; }));
  }
  stop.Cancel();
  EXPECT_TRUE(WaitForEnd());
  {
    std::lock_guard lock(state.mutex);
    state.hold_producer = false;
    state.changed.notify_all();
  }
  const auto deadline = std::chrono::steady_clock::now() + 5s;
  while (slots > 1 && !ModelStreamerRestoreTestPeer::AllPendingWithdrawn(restore) &&
         std::chrono::steady_clock::now() < deadline)
    std::this_thread::yield();
  if (slots > 1) {
    EXPECT_TRUE(ModelStreamerRestoreTestPeer::AllPendingWithdrawn(restore));
  }
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "withdraw/0"));
  ReleaseEnd();
  ExpectCancellation(result, CancellationError::Reason::CANCELLED);
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(HasMapping(root_ / "withdraw"));
}

TEST_P(PipelineCapacityTest, EmptyFilesPartialAndOversizedBatchesPreserveContents)
{
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(3, GetParam()));
  EXPECT_NO_THROW(restore.Stage({}, root_ / "empty"));
  auto empty_files = Inventory(3);
  for (auto& file : empty_files.files)
    file.size_bytes = 0;
  EXPECT_NO_THROW(restore.Stage(empty_files, root_ / "empty-files"));
  EXPECT_EQ(state.submitted, 0U);
  auto small = Inventory();
  EXPECT_NO_THROW(restore.Stage(small, root_ / "fewer-than-slots"));
  auto inventory = Inventory(10);
  const std::vector<uintmax_t> sizes{0, 2, 1, 0, 7, 1, 0, 2, 1, 0};
  for (unsigned i = 0; i < sizes.size(); ++i) {
    inventory.files[i].size_bytes = sizes[i];
    inventory.files[i].permissions = fs::perms::owner_read;
  }
  for (unsigned trial = 0; trial < 2; ++trial) {
    const auto destination = root_ / ("mixed-" + std::to_string(trial));
    ASSERT_NO_THROW(restore.Stage(inventory, destination));
    for (const auto& file : inventory.files) {
      const auto path = destination / file.relative_path;
      EXPECT_EQ(fs::file_size(path), file.size_bytes);
      EXPECT_EQ(fs::status(path).permissions(), file.permissions);
      if (file.size_bytes) {
        char data = 0;
        std::ifstream(path).get(data);
        EXPECT_EQ(data, 'd');
      }
    }
  }
  EXPECT_EQ(state.starts, 1U);
  EXPECT_EQ(state.ends, 0U);
  EXPECT_FALSE(restore.Failed());
  EXPECT_FALSE(HasMapping(root_));
}

INSTANTIATE_TEST_SUITE_P(Slots, PipelineCapacityTest, ::testing::Values(1U, 2U, 3U, 4U, 20U));

TEST_F(ModelStreamerSessionTest, RejectsInvalidPreparationCapacityBeforeStartingWorker)
{
  for (unsigned slots : {0U, 21U})
    EXPECT_THROW((ModelStreamerRestore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, slots))),
                 std::invalid_argument);
  EXPECT_EQ(state.starts, 0U);
}

class PipelineCancellationTest : public ModelStreamerSessionTest,
                                 public ::testing::WithParamInterface<bool> {};
TEST_P(PipelineCancellationTest, BlockedPreparationStillDrainsClaimedWork)
{
  state.allow_responses = false;
  state.hold_allocation = 2;
  state.hold_end = true;
  Cancellation stop;
  const auto deadline = Cancellation::Clock::now() + 200ms;
  const auto cancellation = GetParam() ? stop.WithDeadline(deadline) : stop;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1));
  auto result = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(4), root_ / "cancel-prepare", cancellation);
  });
  EXPECT_TRUE(WaitForSubmissions(1));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 2; }));
  }
  if (!GetParam())
    stop.Cancel();
  EXPECT_TRUE(WaitForEnd());
  {
    std::lock_guard lock(state.mutex);
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "cancel-prepare/0"));
  ReleaseEnd();
  ExpectCancellation(result, GetParam() ? CancellationError::Reason::DEADLINE_EXCEEDED :
                                        CancellationError::Reason::CANCELLED);
  EXPECT_EQ(state.mappings, 1U);
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(fs::exists(root_ / "cancel-prepare/2"));
  EXPECT_FALSE(HasMapping(root_ / "cancel-prepare"));
}
INSTANTIATE_TEST_SUITE_P(CancellationCauses, PipelineCancellationTest, ::testing::Bool(),
    [](const ::testing::TestParamInfo<bool>& info) { return info.param ? "Deadline" : "ExplicitStop"; });

TEST_F(ModelStreamerSessionTest, PipelineKnownFailureStopsPartiallyPreparedBatch)
{
  state.hold_allocation = 2;
  state.allow_responses = false;
  state.hold_end = true;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1));
  auto result = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(4), root_ / "known-failure");
  });
  EXPECT_TRUE(WaitForSubmissions(1));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 2; }));
    state.fault = Fault::UNKNOWN;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  ReleaseEnd();
  ModelStreamerRestoreTestPeer::JoinFailedWorker(restore);
  {
    std::lock_guard lock(state.mutex);
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  try {
    result.get();
    ADD_FAILURE() << "native failure was lost";
  }
  catch (const std::runtime_error& error) {
    EXPECT_EQ(std::string(error.what()), "Model Streamer session failed: injected native status");
  }
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(fs::exists(root_ / "known-failure/2"));
  EXPECT_FALSE(HasMapping(root_ / "known-failure"));
}

class PipelineReadyFailureTest : public ModelStreamerSessionTest,
                                public ::testing::WithParamInterface<bool> {};
TEST_P(PipelineReadyFailureTest, RetiresTwoFailedFuturesWithObservedErrorPrecedence)
{
  state.hold_producer = true;
  state.allow_responses = false;
  state.hold_end = true;
  // Hold exactly two published futures before injecting their failures.
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, 2));
  Cancellation parent;
  auto result = std::async(std::launch::async, [&] {
    producer_restore = &restore;
    restore.Stage(Inventory(4), root_ / "two-failures", parent);
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.producer_at_window; }));
    state.fault = Fault::UNKNOWN;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  ReleaseEnd();
  ModelStreamerRestoreTestPeer::JoinFailedWorker(restore);
  // The producer is inside Reason(), after its last readiness probes. A stop
  // observed here wins; otherwise its next probe selects the older native error.
  if (GetParam())
    parent.Cancel();
  {
    std::lock_guard lock(state.mutex);
    state.hold_producer = false;
    state.changed.notify_all();
  }
  try {
    result.get();
    ADD_FAILURE() << "native failure was lost";
  }
  catch (const std::runtime_error& error) {
    EXPECT_EQ(std::string(error.what()), GetParam() ? "operation cancelled" :
        "Model Streamer session failed: injected native status");
  }
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(fs::exists(root_ / "two-failures/2"));
  EXPECT_FALSE(HasMapping(root_ / "two-failures"));
}
INSTANTIATE_TEST_SUITE_P(ObservationOrder, PipelineReadyFailureTest, ::testing::Bool(),
    [](const ::testing::TestParamInfo<bool>& info) { return info.param ? "StopFirst" : "NativeFirst"; });

TEST_F(ModelStreamerSessionTest, PipelinePreparationFailureDoesNotCancelLivePeerScope)
{
  state.hold_allocation = 2;
  state.fail_allocation = 2;
  state.allow_responses = false;
  state.hold_end = true;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(2));
  auto inventory = Inventory(3);
  inventory.files[1].size_bytes = 2;
  Cancellation first_stop, peer_stop;
  auto first = std::async(std::launch::async, [&] {
    restore.Stage(inventory, root_ / "failed-stage", first_stop);
  });
  EXPECT_TRUE(WaitForSubmissions(1));
  {
    std::unique_lock lock(state.mutex);
    // Ensure the intended restore owns the injected failure before its peer allocates.
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 2; }));
  }
  auto peer = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "peer", peer_stop);
  });
  EXPECT_TRUE(WaitForSubmissions(2));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 2; }));
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForEnd());
  EXPECT_FALSE(first_stop.IsCancelled());
  EXPECT_FALSE(peer_stop.IsCancelled());
  EXPECT_EQ(peer.wait_for(0ms), std::future_status::timeout);
  ReleaseEnd();
  EXPECT_THROW(first.get(), std::system_error);
  try {
    peer.get();
    ADD_FAILURE() << "peer completed despite shared native teardown";
  }
  catch (const std::runtime_error& error) {
    EXPECT_EQ(std::string(error.what()),
        "Model Streamer session interrupted by another submission's cancellation");
  }
  EXPECT_FALSE(HasMapping(root_ / "failed-stage"));
  EXPECT_FALSE(HasMapping(root_ / "peer"));
}

TEST_F(ModelStreamerSessionTest, PipelineEntryAllocationFailureStillDrainsFront)
{
  state.allow_responses = false;
  state.hold_end = true;
  state.arm_slot_failure = true;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1));
  Cancellation parent;
  auto result = std::async(std::launch::async, [&] {
    producer_restore = &restore;
    restore.Stage(Inventory(4), root_ / "slot-failure", parent);
  });
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "slot-failure/0"));
  EXPECT_FALSE(fs::exists(root_ / "slot-failure/1"));
  EXPECT_FALSE(parent.IsCancelled());
  ReleaseEnd();
  EXPECT_THROW(result.get(), std::bad_alloc);
  EXPECT_FALSE(HasMapping(root_ / "slot-failure"));
}

TEST_F(ModelStreamerSessionTest, PipelineRegistrationAllocationFailureDrainsAcceptedNativePointers)
{
  state.fail_registration = true;
  state.hold_end = true;
  ModelStreamerRestore restore;
  auto result = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "registration-failure");
  });
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "registration-failure/0"));
  ReleaseEnd();
  EXPECT_THROW(result.get(), std::bad_alloc);
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_TRUE(restore.Failed());
  EXPECT_FALSE(HasMapping(root_ / "registration-failure"));
}

TEST_F(ModelStreamerSessionTest, PipelineFrontCompletionDuringBlockedAllocationIsConsumedOnce)
{
  state.hold_allocation = 2;
  state.allow_responses = false;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, 3));
  auto inventory = Inventory(5);
  auto result = std::async(std::launch::async, [&] {
    restore.Stage(inventory, root_ / "completion-during-prepare");
  });
  EXPECT_TRUE(WaitForSubmissions(1));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 2; }));
    state.allow_responses = true;
    state.changed.notify_all();
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.completed_submissions == 1; }));
  }
  // This worker barrier settles after the response was processed, while the
  // caller is still in B's syscall. B and C have never been queued.
  auto settled = ModelStreamerRestoreTestPeer::QueueCancelledBarrier(restore);
  EXPECT_EQ(settled.wait_for(5s), std::future_status::ready);
  EXPECT_THROW(settled.get(), CancellationError);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 1U);
    EXPECT_EQ(state.allocation_calls, 2U);
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  EXPECT_NO_THROW(result.get());
  EXPECT_EQ(state.submitted, 5U);
  for (unsigned i = 0; i < inventory.files.size(); ++i)
    EXPECT_EQ(state.submission_sources.at(i), std::vector<std::string>{inventory.files[i].source_locator});
  EXPECT_FALSE(HasMapping(root_));
  EXPECT_EQ(state.starts, 1U);
  EXPECT_EQ(state.ends, 0U);
}

TEST_F(ModelStreamerSessionTest, PipelinePendingFrontCancellationReleasesTailsDuringPeerTeardown)
{
  state.allow_responses = false;
  state.hold_end = true;
  state.hold_allocation = 4;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, 3));
  Cancellation active_stop, pending_stop;
  auto active = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "active", active_stop);
  });
  EXPECT_TRUE(WaitForSubmissions(1));
  auto pending = std::async(std::launch::async, [&] {
    PreparedRestore preparation;
    preparation.inventory = Inventory(5);
    restore.Stage(preparation.inventory, root_ / "pending", pending_stop);
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 4; }));
    EXPECT_TRUE(ModelStreamerRestoreTestPeer::HasOnlyUnclaimedPending(restore, 2));
  }
  active_stop.Cancel();
  EXPECT_TRUE(WaitForEnd());
  pending_stop.Cancel();
  {
    std::lock_guard lock(state.mutex);
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  EXPECT_EQ(pending.wait_for(5s), std::future_status::ready);
  ExpectCancellation(pending, CancellationError::Reason::CANCELLED);
  EXPECT_EQ(ModelStreamerRestoreTestPeer::PendingCount(restore), 2U);
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::AllPendingWithdrawn(restore));
  EXPECT_FALSE(HasMapping(root_ / "pending"));
  EXPECT_TRUE(HasMapping(root_ / "active"));
  EXPECT_EQ(active.wait_for(0ms), std::future_status::timeout);
  ReleaseEnd();
  ExpectCancellation(active, CancellationError::Reason::CANCELLED);
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(HasMapping(root_));
}

TEST_F(ModelStreamerSessionTest, PipelineCreationFailureDrainsPublishedAndPartialBatches)
{
  state.allow_responses = false;
  state.hold_allocation = 2;
  state.hold_end = true;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, 4));
  Cancellation parent;
  auto sibling = Cancellation::ChildOf(parent);
  auto result = std::async(std::launch::async, [&] {
    PreparedRestore preparation;
    preparation.inventory = Inventory(5);
    preparation.inventory.files[3].relative_path = std::string(300, 'x');
    restore.Stage(preparation.inventory, root_ / "creation-failure", parent);
  });
  EXPECT_TRUE(WaitForSubmissions(1));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 2; }));
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "creation-failure/0"));
  EXPECT_TRUE(HasMapping(root_ / "creation-failure/1"));
  EXPECT_TRUE(HasMapping(root_ / "creation-failure/2"));
  EXPECT_FALSE(parent.IsCancelled());
  EXPECT_FALSE(sibling.IsCancelled());
  ReleaseEnd();
  try {
    result.get();
    ADD_FAILURE() << "file creation error was lost";
  }
  catch (const std::system_error& error) {
    EXPECT_EQ(error.code(), std::errc::filename_too_long);
  }
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(fs::exists(root_ / "creation-failure/4"));
  EXPECT_FALSE(HasMapping(root_));
}

class PipelineBlockedFrontFailureTest : public ModelStreamerSessionTest,
                                       public ::testing::WithParamInterface<bool> {};
TEST_P(PipelineBlockedFrontFailureTest, StopsPrivatePreparationAfterRecoverableFrontError)
{
  const bool rejection = GetParam();
  state.allow_responses = false;
  state.hold_allocation = 2;
  state.hold_request = rejection;
  state.reject_request = rejection ? 1 : 0;
  state.storage_failure_submission = rejection ? 0 : 1;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, 3));
  Cancellation parent;
  auto result = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(5), root_ / "failed-front", parent);
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] {
      return state.requests_entered == 1 && state.allocation_entered == 2;
    }));
    state.hold_request = false;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  if (!rejection) {
    EXPECT_TRUE(WaitForStopped());
  }
  auto settled = ModelStreamerRestoreTestPeer::QueueCancelledBarrier(restore);
  EXPECT_EQ(settled.wait_for(5s), std::future_status::ready);
  EXPECT_THROW(settled.get(), CancellationError);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.requests_entered, 1U);
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  try {
    result.get();
    ADD_FAILURE() << "front failure was lost";
  }
  catch (const std::runtime_error& error) {
    EXPECT_EQ(std::string(error.what()), rejection ? "submit Model Streamer restore: injected native status" :
                                                   "Model Streamer restore range: injected native status");
  }
  EXPECT_EQ(state.requests_entered, 1U);
  EXPECT_FALSE(fs::exists(root_ / "failed-front/2"));
  EXPECT_FALSE(HasMapping(root_));
  EXPECT_FALSE(parent.IsCancelled());
  EXPECT_FALSE(restore.Failed());
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "retry"));
}
INSTANTIATE_TEST_SUITE_P(Failures, PipelineBlockedFrontFailureTest, ::testing::Bool(),
    [](const ::testing::TestParamInfo<bool>& info) { return info.param ? "Rejection" : "Storage"; });

TEST_F(ModelStreamerSessionTest, PipelineConcurrentCallersKeepIndependentQueues)
{
  state.allow_responses = false;
  state.hold_allocation = 2;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(2, 3));
  auto inventory = Inventory(3);
  inventory.files[1].size_bytes = 2;
  auto peer_inventory = inventory;
  for (auto& file : peer_inventory.files)
    file.source_locator += "-peer";
  auto first = std::async(std::launch::async, [&] { restore.Stage(inventory, root_ / "first"); });
  EXPECT_TRUE(WaitForSubmissions(1));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 2; }));
  }
  auto peer = std::async(std::launch::async, [&] { restore.Stage(peer_inventory, root_ / "peer"); });
  EXPECT_TRUE(WaitForSubmissions(2));
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::WaitForPending(restore, 2));
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.max_active_submissions, 2U);
    EXPECT_EQ(state.max_active_bytes, 2U);
    EXPECT_EQ(ModelStreamerRestoreTestPeer::PendingCount(restore), 2U);
    state.hold_allocation = 0;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_NO_THROW(first.get());
  EXPECT_NO_THROW(peer.get());
  EXPECT_EQ(state.submitted, 6U);
  EXPECT_LE(state.max_active_bytes, 2U);
  EXPECT_EQ(state.starts, 1U);
  EXPECT_EQ(state.ends, 0U);
  EXPECT_FALSE(HasMapping(root_));
}

TEST_F(ModelStreamerSessionTest, PipelineLocalQueueGrowthFailureDrainsAcceptedFront)
{
  const auto block_bytes = ModelStreamerRestoreTestPeer::LocalQueueBlockAllocationSize();
  ASSERT_NE(block_bytes, 0U);
  state.allow_responses = false;
  state.hold_end = true;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, 2));
  auto result = std::async(std::launch::async, [&] {
    PreparedRestore preparation;
    preparation.inventory = Inventory(64);
    producer_restore = &restore;
    fail_allocation_size = block_bytes;
    skip_matching_allocations = 1;  // The initial node precedes publication.
    restore.Stage(preparation.inventory, root_ / "queue-growth");
  });
  {
    std::unique_lock lock(state.mutex);
    for (unsigned prepared = 2; prepared < 64 && !state.queue_growth_failed; ++prepared) {
      // Do not finish the current front until its lookahead is prepared. A node
      // growth fault occurs before that lookahead can allocate its first file.
      EXPECT_TRUE(state.changed.wait_for(lock, 5s, [&] {
        return state.queue_growth_failed || state.allocation_calls >= prepared;
      }));
      if (!state.queue_growth_failed) {
        state.pause_after_completion = true;
        state.allow_responses = true;
        state.changed.notify_all();
      }
    }
    EXPECT_TRUE(state.queue_growth_failed);
  }
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "queue-growth"));
  ReleaseEnd();
  EXPECT_THROW(result.get(), std::bad_alloc);
  EXPECT_FALSE(HasMapping(root_));
}

TEST_F(ModelStreamerSessionTest, PipelineQueueFailureDrainsEarlierClaimedSubmission)
{
  state.allow_responses = false;
  state.hold_request = true;
  state.hold_allocation = 2;
  state.hold_end = true;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1));
  auto inventory = Inventory(4);
  for (auto& file : inventory.files)
    file.permissions = fs::perms::owner_read;
  std::size_t actual_fault_bytes = 0;
  auto result = std::async(std::launch::async, [&] {
    producer_restore = &restore;
    try {
      restore.Stage(inventory, root_ / "queue-failure");
    }
    catch (const std::bad_alloc&) {
      actual_fault_bytes = last_failed_allocation_size;
      throw;
    }
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] {
      return state.requests_entered == 1 && state.allocation_entered == 2;
    }));
  }
  // The worker is held in A's request. Force the next real queue push to grow.
  ModelStreamerRestoreTestPeer::ExpectQueueAllocationFailure(restore, true);
  const auto expected_fault_bytes = last_failed_allocation_size;
  EXPECT_GT(expected_fault_bytes, 0U);
  {
    std::lock_guard lock(state.mutex);
    state.queue_allocation_bytes = expected_fault_bytes;
    state.arm_queue_failure = true;
    state.hold_allocation = 0;
    state.changed.notify_all();
  }
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return !state.arm_queue_failure; }));
    state.hold_request = false;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_TRUE(HasMapping(root_ / "queue-failure/0"));
  ReleaseEnd();
  EXPECT_THROW(result.get(), std::bad_alloc);
  EXPECT_EQ(actual_fault_bytes, expected_fault_bytes);
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_FALSE(fs::exists(root_ / "queue-failure/2"));
  EXPECT_FALSE(HasMapping(root_ / "queue-failure"));
}

TEST_F(ModelStreamerSessionTest, PipelineWorkerStartsNextBatchDuringBlockedPreparation)
{
  state.allow_responses = false;
  state.hold_allocation = 3;
  state.pause_after_completion = true;
  // Exercise the default window: A downloading, B queued, C preparing.
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1));
  auto inventory = Inventory(5);
  auto result = std::async(std::launch::async, [&] {
    restore.Stage(inventory, root_ / "worker-overlap");
  });
  EXPECT_TRUE(WaitForSubmissions(1));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocation_entered == 3; }));
  }
  EXPECT_TRUE(ModelStreamerRestoreTestPeer::WaitForPending(restore, 1));
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 1U);
    EXPECT_EQ(state.mappings, 2U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  // The shared byte budget admits B only after A. The caller is still inside
  // C's allocation, so B's native acceptance must be driven by the worker.
  EXPECT_TRUE(WaitForSubmissions(2));
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.hold_allocation, 3U);
    EXPECT_EQ(state.allocation_calls, 3U);
    EXPECT_EQ(state.mappings, 2U);
    EXPECT_EQ(state.max_active_submissions, 1U);
    EXPECT_EQ(state.submission_sources.size(), 2U);
    if (state.submission_sources.size() == 2) {
      EXPECT_EQ(state.submission_sources[1], std::vector<std::string>{inventory.files[1].source_locator});
    }
    EXPECT_FALSE(HasMapping(root_ / "worker-overlap/2"));
    state.hold_allocation = 0;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_NO_THROW(result.get());
  EXPECT_EQ(state.submitted, 5U);
  EXPECT_FALSE(HasMapping(root_));
}

TEST_F(ModelStreamerSessionTest, PipelineEmptySuffixDoesNotRepublishOutstandingBatches)
{
  state.allow_responses = false;
  state.hold_producer = true;
  state.producer_window_slots = 2;
  ModelStreamerRestore restore({}, kDefaultModelStreamerSubmissionTimeout, RestoreSettings(1, 3));
  auto inventory = Inventory(4);
  for (unsigned i = 2; i < 4; ++i) {
    inventory.files[i].size_bytes = 0;
    inventory.files[i].permissions = fs::perms::owner_read;
  }
  auto result = std::async(std::launch::async, [&] {
    producer_restore = &restore;
    restore.Stage(inventory, root_ / "empty-suffix");
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.producer_at_window; }));
    EXPECT_EQ(state.submitted, 1U);
    EXPECT_EQ(ModelStreamerRestoreTestPeer::PendingCount(restore), 1U);
    for (unsigned i = 2; i < 4; ++i) {
      const auto path = root_ / "empty-suffix" / std::to_string(i);
      EXPECT_EQ(fs::file_size(path), 0U);
      EXPECT_EQ(fs::status(path).permissions(), fs::perms::owner_read);
    }
    state.hold_producer = false;
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_NO_THROW(result.get());
  EXPECT_EQ(state.submission_sources, (std::vector<std::vector<std::string>>{
      {inventory.files[0].source_locator}, {inventory.files[1].source_locator}}));
  EXPECT_FALSE(HasMapping(root_));
}
