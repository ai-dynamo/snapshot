// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"

#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <filesystem>
#include <fstream>
#include <future>
#include <map>
#include <mutex>
#include <string>
#include <system_error>
#include <thread>
#include <unistd.h>
#include <vector>

#include <streamer/streamer.h>

#include "tests/temporary_directory.hpp"

using namespace snapshot::pagebroker;
using namespace std::chrono_literals;
namespace fs = std::filesystem;

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

  static bool HasWorker(const ModelStreamerRestore& restore) { return restore.worker_.joinable(); }
  static void RequestStop(ModelStreamerRestore& restore) { restore.RequestStop(); }
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
    bool failed = false;
  };
  std::vector<Response> responses;
  std::map<RunaiFileStreamerSubmissionId, Submission> entries;
  std::size_t next = 0;
  unsigned submissions = 0;
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
  unsigned completed_submissions = 0;
  int allocation_error = 0;
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
  unsigned response_polls = 0;
  unsigned ends_entered = 0;
  unsigned ends = 0;
  unsigned submitted = 0;
  unsigned max_submissions = 0;
  std::vector<std::string> sources;
  std::vector<std::vector<std::string>> submission_sources;
  std::vector<uintmax_t> submission_bytes;
  std::thread::id native_thread;
  Fault fault = Fault::NONE;
  RunaiFileStreamerSubmissionId next_id = 0;
} state;

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
    state.completed_submissions = 0;
    state.allocation_error = 0;
    state.start_status = state.credential_status = 0;
    state.request_status = RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
    state.return_submission_id = true;
    state.storage_failure_submission = 0;
    state.return_start_handle = true;
    state.sparse_allocations = false;
    state.starts = state.ends_entered = state.ends = state.submitted = state.max_submissions = 0;
    state.allocations = state.requests_entered = 0;
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
  bool WaitForIdle()
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

extern "C" int __wrap_posix_fallocate(int descriptor, off_t offset, off_t length)
{
  {
    std::lock_guard lock(state.mutex);
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
    for (auto& [id, entry] : session->entries)
      for (auto* destination : entry.destinations)
        *destination = 'e';
    state.max_submissions = std::max(state.max_submissions, session->submissions);
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
  if (state.request_status != RUNAI_FILE_STREAMER_RESPONSE_SUCCESS &&
      state.request_status != RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR && !state.return_submission_id) {
    *id = 0;
    return state.request_status;
  }
  const auto native_id = ++state.next_id;
  *id = state.return_submission_id ? native_id : 0;
  ++state.submitted;
  ++session.submissions;
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
    session.responses.push_back(Response{native_id, file, file + 1 == files ? 1 : 0});
  }
  state.changed.notify_all();
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

TEST_F(ModelStreamerSessionTest, ReusesWorkerAfterEachSessionDrains)
{
  ModelStreamerRestore restore;
  std::thread::id worker;
  for (unsigned i = 0; i < 8; ++i) {
    EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / std::to_string(i)));
    EXPECT_TRUE(WaitForIdle());
    EXPECT_TRUE(ModelStreamerRestoreTestPeer::HasWorker(restore));
    std::lock_guard lock(state.mutex);
    if (i == 0)
      worker = state.native_thread;
    EXPECT_EQ(state.native_thread, worker);
    EXPECT_EQ(state.starts, i + 1);
    EXPECT_EQ(state.ends, i + 1);
  }
  // Repeated requests to stop must wake the idle worker and remain safe when
  // the destructor requests stop again and joins it.
  ModelStreamerRestoreTestPeer::RequestStop(restore);
  ModelStreamerRestoreTestPeer::RequestStop(restore);
  EXPECT_THROW(restore.Stage(Inventory(), root_ / "stopped"), std::runtime_error);
  EXPECT_FALSE(restore.Failed());
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
  EXPECT_TRUE(WaitForIdle());
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

TEST_F(ModelStreamerSessionTest, SubmitsOversizedFileAloneAndContinuesInventory)
{
  state.sparse_allocations = true;
  auto inventory = Inventory(2);
  inventory.files[0].size_bytes = 10'000'000'001;
  ModelStreamerRestore restore;
  ASSERT_NO_THROW(restore.Stage(inventory, root_ / "oversized"));
  EXPECT_TRUE(WaitForIdle());
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

TEST_F(ModelStreamerSessionTest, LaterBatchFailureReleasesMappingsAndStopsInventory)
{
  state.sparse_allocations = true;
  state.storage_failure_submission = 2;
  state.hold_end = true;
  state.hold_end_after_submission = 2;
  auto inventory = Inventory(3);
  for (auto& file : inventory.files)
    file.size_bytes = 6'000'000'000;
  ModelStreamerRestore restore;
  auto result = std::async(std::launch::async, [&] { restore.Stage(inventory, root_ / "partial"); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.ends_entered == 2; }));
  }
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  EXPECT_FALSE(HasMapping(root_ / "partial/0"));
  EXPECT_TRUE(HasMapping(root_ / "partial/1"));
  EXPECT_FALSE(fs::exists(root_ / "partial/2"));
  ReleaseEnd();
  EXPECT_THROW(result.get(), std::runtime_error);
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

TEST_F(ModelStreamerSessionTest, SuccessfulSubmissionReleasesMappingsBeforeEndReturns)
{
  state.hold_end = true;
  ModelStreamerRestore restore;
  auto result = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "restore"); });
  const bool ending = WaitForEnd();
  EXPECT_TRUE(ending);
  const bool returned = result.wait_for(2s) == std::future_status::ready;
  EXPECT_TRUE(returned);
  if (returned) {
    EXPECT_FALSE(HasMapping(root_ / "restore"));
  }
  ReleaseEnd();
  ASSERT_NO_THROW(result.get());
  char data = 0;
  std::ifstream(root_ / "restore" / "0").get(data);
  EXPECT_EQ(data, 'd');
  EXPECT_TRUE(WaitForIdle());
  EXPECT_EQ(state.ends, 1U);
  EXPECT_FALSE(HasMapping(root_ / "restore"));
  EXPECT_FALSE(restore.Failed());
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

TEST_F(ModelStreamerSessionTest, StartsFreshSessionsWithoutRetainingCompletedMappings)
{
  ModelStreamerRestore restore;
  for (unsigned i = 0; i < 20; ++i) {
    ASSERT_NO_THROW(restore.Stage(Inventory(), root_ / std::to_string(i)));
    EXPECT_TRUE(WaitForIdle());
    EXPECT_EQ(state.ends, i + 1);
    EXPECT_EQ(state.starts, state.ends);
    EXPECT_FALSE(HasMapping(root_ / std::to_string(i)));
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
  EXPECT_TRUE(WaitForEnd());
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
  EXPECT_TRUE(WaitForIdle());
  EXPECT_EQ(state.response_polls, 0U);
  if (!failure.terminal) {
    {
      std::lock_guard lock(state.mutex);
      state.request_status = RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
      state.return_submission_id = true;
    }
    EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "retry"));
    EXPECT_TRUE(WaitForIdle());
    EXPECT_EQ(state.starts, 2U);
    EXPECT_EQ(state.ends, 2U);
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
  EXPECT_TRUE(WaitForIdle());
  EXPECT_EQ(state.ends, 2U);
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

TEST_F(ModelStreamerSessionTest, BoundsSessionAdmissionUnderConcurrentLoad)
{
  state.allow_responses = false;
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
  for (auto& result : results)
    EXPECT_NO_THROW(result.get());
  EXPECT_TRUE(WaitForIdle());
  EXPECT_LE(state.max_submissions, 8U);
  EXPECT_EQ(state.submitted, 20U);
  EXPECT_EQ(state.starts, state.ends);
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
  EXPECT_TRUE(WaitForIdle());
  EXPECT_FALSE(restore.Failed());
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "next-session"));
  EXPECT_EQ(state.starts, 2U);
}

TEST_F(ModelStreamerSessionTest, CompletedFailureStopsAdmissionUntilSessionDrains)
{
  state.fault = Fault::STORAGE;
  state.allow_responses = false;
  state.pause_after_completion = true;
  ModelStreamerRestore restore;
  auto first = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "first"); });
  EXPECT_TRUE(WaitForSubmissions(1));
  auto second = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "second"); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted == 2; }));
    state.allow_responses = true;
    state.changed.notify_all();
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.completed_submissions == 1; }));
  }
  // One batch has failed but the other still holds the session open. A new
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

TEST_F(ModelStreamerSessionTest, BoundsSessionBytesAcrossSubmissions)
{
  state.sparse_allocations = true;
  state.allow_responses = false;
  ModelStreamerRestore restore;
  auto inventory = Inventory();
  // Sparse files exercise admission without allocating or reading gigabytes.
  inventory.files.front().size_bytes = 6'000'000'000;
  std::vector<std::future<void>> results;
  for (unsigned i = 0; i < 3; ++i)
    results.push_back(std::async(std::launch::async, [&, i] { restore.Stage(inventory, root_ / std::to_string(i)); }));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted != 0; }));
    EXPECT_EQ(state.submitted, 1U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  for (auto& result : results)
    EXPECT_NO_THROW(result.get());
  EXPECT_TRUE(WaitForIdle());
  EXPECT_EQ(state.max_submissions, 1U);
  EXPECT_EQ(state.starts, 3U);
  EXPECT_EQ(state.ends, 3U);
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

TEST_F(ModelStreamerSessionTest, QueuedDeadlineDoesNotWaitForOrStopActiveReads)
{
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
  cancellation = cancellation.WithDeadline(Cancellation::Clock::now() + 50ms);
  auto queued = std::async(std::launch::async, [&] { restore.Stage(inventory, root_ / "queued", cancellation); });
  const auto queued_status = queued.wait_for(2s);
  EXPECT_EQ(queued_status, std::future_status::ready);
  EXPECT_EQ(active.wait_for(0ms), std::future_status::timeout);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 1U);
    EXPECT_EQ(state.ends, 0U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_THROW(queued.get(), CancellationError);
  EXPECT_NO_THROW(active.get());
  EXPECT_FALSE(restore.Failed());
}

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

TEST_F(ModelStreamerSessionTest, CancellationDuringStartupDoesNotFailCoordinator)
{
  state.hold_start = true;
  Cancellation stop;
  ModelStreamerRestore restore;
  Cancellation cancellation;
  cancellation = stop;
  auto cancelled = std::async(std::launch::async, [&] {
    restore.Stage(Inventory(), root_ / "cancelled", cancellation);
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.starts == 1; }));
  }
  stop.Cancel();
  EXPECT_EQ(cancelled.wait_for(2s), std::future_status::ready);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 0U);
    state.hold_start = false;
    state.changed.notify_all();
  }
  EXPECT_THROW(cancelled.get(), CancellationError);
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_FALSE(restore.Failed());
  EXPECT_NO_THROW(restore.Stage(Inventory(), root_ / "success"));
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_TRUE(WaitForIdle());
  EXPECT_EQ(state.starts, state.ends);
}

// Exercise explicit cancellation and deadline expiry while end() blocks the worker.
class QueuedTeardownCancellationTest : public ModelStreamerSessionTest,
                                       public ::testing::WithParamInterface<bool> {};

TEST_P(QueuedTeardownCancellationTest, ReturnsBeforeTeardownAndNeverSubmitsCancelledBuffers)
{
  const bool deadline = GetParam();
  state.hold_end = true;
  ModelStreamerRestore restore;
  auto active = std::async(std::launch::async, [&] { restore.Stage(Inventory(), root_ / "active"); });
  EXPECT_TRUE(WaitForEnd());
  const bool active_returned = active.wait_for(2s) == std::future_status::ready;
  EXPECT_TRUE(active_returned);

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
  if (active_returned) {
    EXPECT_FALSE(HasMapping(root_ / "active"));
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
  EXPECT_NO_THROW(active.get());
  ExpectCancellation(queued, deadline ? CancellationError::Reason::DEADLINE_EXCEEDED :
                                       CancellationError::Reason::CANCELLED);
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
