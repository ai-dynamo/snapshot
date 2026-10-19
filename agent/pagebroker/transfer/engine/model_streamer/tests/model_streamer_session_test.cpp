// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"

#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <condition_variable>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <future>
#include <mutex>
#include <string>
#include <system_error>
#include <unistd.h>
#include <vector>

#include <streamer/streamer.h>

using namespace snapshot::pagebroker;
using namespace std::chrono_literals;
namespace fs = std::filesystem;

namespace {
enum class Fault { NONE, UNKNOWN, COMMITTED_REQUEST, INVALID_INDEX, EARLY_DONE, MISSING_DONE, STORAGE, TIMEOUT };
struct Response {
  RunaiFileStreamerSubmissionId id;
  unsigned file;
  int done;
};
struct Session {
  std::vector<Response> responses;
  std::vector<char*> destinations;
  std::size_t next = 0;
  unsigned submissions = 0;
};
struct FakeState {
  std::mutex mutex;
  std::condition_variable changed;
  bool hold_end = false;
  bool hold_start = false;
  bool hold_request = false;
  bool allow_responses = true;
  int allocation_error = 0;
  int start_status = 0;
  bool return_start_handle = true;
  int credential_status = 0;
  bool sparse_allocations = false;
  unsigned starts = 0;
  unsigned allocations = 0;
  unsigned requests_entered = 0;
  unsigned ends_entered = 0;
  unsigned ends = 0;
  unsigned submitted = 0;
  unsigned max_submissions = 0;
  Fault fault = Fault::NONE;
  RunaiFileStreamerSubmissionId next_id = 0;
} state;

class ModelStreamerSessionTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    state.hold_end = false;
    state.hold_start = false;
    state.hold_request = false;
    state.allow_responses = true;
    state.allocation_error = 0;
    state.start_status = state.credential_status = 0;
    state.return_start_handle = true;
    state.sparse_allocations = false;
    state.starts = state.ends_entered = state.ends = state.submitted = state.max_submissions = 0;
    state.allocations = state.requests_entered = 0;
    state.fault = Fault::NONE;
    state.next_id = 0;
    char path[] = "/tmp/pagebroker-session-XXXXXX";
    const char* created = mkdtemp(path);
    ASSERT_NE(created, nullptr);
    root_ = created;
  }
  void TearDown() override { fs::remove_all(root_); }

  RestorePlan Plan(unsigned files = 1) const
  {
    RestorePlan plan;
    for (unsigned i = 0; i < files; ++i)
      plan.files.push_back(RestoreFile{"fake-source", std::to_string(i), 1});
    return plan;
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
  *value = state.return_start_handle ? new Session : nullptr;
  ++state.starts;
  state.changed.notify_all();
  state.changed.wait(lock, [] { return !state.hold_start; });
  return state.start_status;
}

extern "C" int runai_file_streamer_set_credentials(void*, const char**, const char**, unsigned)
{
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
    ++state.ends_entered;
    state.changed.notify_all();
    state.changed.wait(lock, [] { return !state.hold_end; });
    // Model the documented native access after the final response, including
    // an UnknownError. Unmapping before end returns makes this write invalid.
    for (auto* destination : session->destinations)
      *destination = 'e';
    state.max_submissions = std::max(state.max_submissions, session->submissions);
    ++state.ends;
  }
  delete session;
}

extern "C" int runai_file_streamer_request(
    void* value, RunaiFileStreamerSubmissionId* id, unsigned files, const char**,
    unsigned* ranges, std::size_t* offsets, std::size_t* sizes, void** destinations,
    RunaiFileStreamerDevice device)
{
  EXPECT_EQ(device.type, RUNAI_FILE_STREAMER_DEVICE_CPU);
  EXPECT_EQ(device.id, 0);
  auto& session = *static_cast<Session*>(value);
  std::unique_lock lock(state.mutex);
  ++state.requests_entered;
  state.changed.notify_all();
  state.changed.wait(lock, [] { return !state.hold_request; });
  *id = ++state.next_id;
  ++state.submitted;
  ++session.submissions;
  for (unsigned file = 0; file < files; ++file) {
    EXPECT_EQ(ranges[file], 1U);
    EXPECT_EQ(offsets[file], 0U);
    EXPECT_GT(sizes[file], 0U);
    auto* destination = static_cast<char*>(destinations[file]);
    *destination = 'd';
    session.destinations.push_back(destination);
    session.responses.push_back(Response{*id, file, file + 1 == files ? 1 : 0});
  }
  state.changed.notify_all();
  return state.fault == Fault::COMMITTED_REQUEST ? RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR :
      RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
}

extern "C" int runai_file_streamer_response(
    void* value, RunaiFileStreamerSubmissionId* id, unsigned* file, unsigned* range, int* done, unsigned)
{
  auto& session = *static_cast<Session*>(value);
  std::unique_lock lock(state.mutex);
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
  if (state.fault == Fault::EARLY_DONE)
    *done = 1;
  if (state.fault == Fault::MISSING_DONE)
    *done = 0;
  if (state.fault == Fault::UNKNOWN)
    return RUNAI_FILE_STREAMER_RESPONSE_UNKNOWN_ERROR;
  if (state.fault == Fault::STORAGE)
    return RUNAI_FILE_STREAMER_RESPONSE_FILE_ACCESS_ERROR;
  return RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
}

extern "C" const char* runai_file_streamer_response_str(int) { return "injected native status"; }

TEST_F(ModelStreamerSessionTest, AllocationFailureDoesNotSubmitNativeWritesAndAllowsRetry)
{
  state.allocation_error = ENOSPC;
  ModelStreamerRestore restore;
  try {
    restore.Stage(Plan(), root_ / "full");
    FAIL() << "restore accepted a destination without backing storage";
  }
  catch (const std::system_error& error) {
    EXPECT_EQ(error.code(), std::errc::no_space_on_device);
  }
  EXPECT_EQ(state.starts, 0U);
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_FALSE(restore.Failed());

  state.allocation_error = 0;
  EXPECT_NO_THROW(restore.Stage(Plan(), root_ / "retry"));
  EXPECT_EQ(state.submitted, 1U);
}

enum class StartupFault { ErrorWithHandle, ErrorWithoutHandle, MissingHandle, Configuration };
class ModelStreamerStartupFailureTest : public ModelStreamerSessionTest,
                                       public ::testing::WithParamInterface<StartupFault> {};

TEST_P(ModelStreamerStartupFailureTest, ReleasesHandleAndPermitBeforeReportingFailure)
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
    EXPECT_THROW(restore.Stage(Plan(), root_ / "failed"), std::runtime_error);
    EXPECT_TRUE(restore.Failed());
    EXPECT_EQ(state.ends, expected_ends);
    EXPECT_EQ(state.requests_entered, 0U);
    state.start_status = state.credential_status = 0;
    state.return_start_handle = true;
    // Keep the failed coordinator alive: the replacement must not depend on
    // its destructor to release the process-wide native-session permit.
    ModelStreamerRestore replacement(options);
    EXPECT_NO_THROW(replacement.Stage(Plan(), root_ / "retry",
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

TEST_F(ModelStreamerSessionTest, KeepsSuccessfulDestinationsAliveUntilEndReturns)
{
  state.hold_end = true;
  ModelStreamerRestore restore;
  auto result = std::async(std::launch::async, [&] { restore.Stage(Plan(), root_ / "restore"); });
  const bool ending = WaitForEnd();
  EXPECT_TRUE(ending);
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  ReleaseEnd();
  ASSERT_NO_THROW(result.get());
  char data = 0;
  std::ifstream(root_ / "restore" / "0").get(data);
  EXPECT_EQ(data, 'e');
  EXPECT_EQ(state.ends, 1U);
  EXPECT_FALSE(restore.Failed());
}

TEST_F(ModelStreamerSessionTest, DrainsBeforeReleasingTerminalFailures)
{
  for (const auto fault : {Fault::UNKNOWN, Fault::COMMITTED_REQUEST, Fault::INVALID_INDEX,
                            Fault::EARLY_DONE, Fault::MISSING_DONE, Fault::TIMEOUT}) {
    SCOPED_TRACE(static_cast<int>(fault));
    state.fault = fault;
    state.hold_end = true;
    state.ends_entered = 0;
    ModelStreamerRestore restore(30ms);
    auto result = std::async(std::launch::async, [&] {
      restore.Stage(Plan(2), root_ / std::to_string(static_cast<int>(fault)));
    });
    const bool ending = WaitForEnd();
    EXPECT_TRUE(ending);
    EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
    ReleaseEnd();
    EXPECT_THROW(result.get(), std::runtime_error);
    EXPECT_TRUE(restore.Failed());
  }
}

TEST_F(ModelStreamerSessionTest, StartsFreshSessionsWithoutRetainingCompletedMappings)
{
  ModelStreamerRestore restore;
  for (unsigned i = 0; i < 20; ++i) {
    ASSERT_NO_THROW(restore.Stage(Plan(), root_ / std::to_string(i)));
    EXPECT_EQ(state.ends, i + 1);
    EXPECT_EQ(state.starts, state.ends);
  }
}

TEST_F(ModelStreamerSessionTest, StorageFailureAllowsSubsequentRestore)
{
  ModelStreamerRestore restore;
  state.fault = Fault::STORAGE;
  EXPECT_THROW(restore.Stage(Plan(), root_ / "failed"), std::runtime_error);
  EXPECT_FALSE(restore.Failed());
  EXPECT_EQ(state.ends, 1U);
  state.fault = Fault::NONE;
  EXPECT_NO_THROW(restore.Stage(Plan(), root_ / "success"));
  EXPECT_EQ(state.ends, 2U);
}

TEST_F(ModelStreamerSessionTest, BoundsSessionAdmissionUnderConcurrentLoad)
{
  state.allow_responses = false;
  ModelStreamerRestore restore;
  std::vector<std::future<void>> results;
  for (unsigned i = 0; i < 20; ++i)
    results.push_back(std::async(std::launch::async, [&, i] { restore.Stage(Plan(), root_ / std::to_string(i)); }));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted >= 8; }));
    EXPECT_EQ(state.submitted, 8U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  for (auto& result : results)
    EXPECT_NO_THROW(result.get());
  EXPECT_LE(state.max_submissions, 8U);
  EXPECT_EQ(state.submitted, 20U);
  EXPECT_EQ(state.starts, state.ends);
}

TEST_F(ModelStreamerSessionTest, BoundsSessionBytesAcrossSubmissions)
{
  state.sparse_allocations = true;
  state.allow_responses = false;
  ModelStreamerRestore restore;
  auto plan = Plan();
  // Sparse files exercise admission without allocating or reading gigabytes.
  plan.files.front().size_bytes = 6'000'000'000;
  std::vector<std::future<void>> results;
  for (unsigned i = 0; i < 3; ++i)
    results.push_back(std::async(std::launch::async, [&, i] { restore.Stage(plan, root_ / std::to_string(i)); }));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted != 0; }));
    EXPECT_EQ(state.submitted, 1U);
    state.allow_responses = true;
    state.changed.notify_all();
  }
  for (auto& result : results)
    EXPECT_NO_THROW(result.get());
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
    results.push_back(std::async(std::launch::async, [&, i] { restore.Stage(Plan(), root_ / std::to_string(i)); }));
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted >= 8; }));
    state.allow_responses = true;
    state.changed.notify_all();
  }
  EXPECT_TRUE(WaitForEnd());
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
  auto plan = Plan();
  plan.files.front().size_bytes = 6'000'000'000;
  auto active = std::async(std::launch::async, [&] { restore.Stage(plan, root_ / "active"); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted == 1; }));
  }
  Cancellation control;
  control = control.WithDeadline(Cancellation::Clock::now() + 50ms);
  auto queued = std::async(std::launch::async, [&] { restore.Stage(plan, root_ / "queued", control); });
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

TEST_F(ModelStreamerSessionTest, ActiveCancellationWaitsForNativeTeardown)
{
  state.allow_responses = false;
  state.hold_end = true;
  Cancellation stop;
  ModelStreamerRestore restore;
  Cancellation control;
  control = stop;
  auto result = std::async(std::launch::async, [&] { restore.Stage(Plan(), root_ / "restore", control); });
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
  auto first = std::async(std::launch::async, [&] { restore.Stage(Plan(), root_ / "first", trigger); });
  auto live = std::async(std::launch::async, [&] { restore.Stage(Plan(), root_ / "live"); });
  auto cancelled = std::async(std::launch::async, [&] {
    restore.Stage(Plan(), root_ / "cancelled", cancelled_peer);
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
  // Post rejects new work with the event loop's retained exception while end
  // is blocked. It must apply the same per-request attribution as active work.
  if (ending)
    expect_session_failure([&] { restore.Stage(Plan(), root_ / "late"); });
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
  Cancellation control;
  control = stop;
  auto cancelled = std::async(std::launch::async, [&] {
    restore.Stage(Plan(), root_ / "cancelled", control);
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
  EXPECT_NO_THROW(restore.Stage(Plan(), root_ / "success"));
  EXPECT_EQ(state.submitted, 1U);
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
  auto active = std::async(std::launch::async, [&] { restore.Stage(Plan(), root_ / "active"); });
  EXPECT_TRUE(WaitForEnd());

  Cancellation control;
  if (deadline)
    control = control.WithDeadline(Cancellation::Clock::now() + 1s);
  auto queued = std::async(std::launch::async, [&] { restore.Stage(Plan(), root_ / "queued", control); });
  {
    std::unique_lock lock(state.mutex);
    // The final file's allocation follows the last preparation cancellation
    // check. From here this batch proceeds to the submission wait.
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.allocations == 2; }));
  }
  if (!deadline)
    control.Cancel();
  const auto queued_status = queued.wait_for(2s);
  EXPECT_EQ(queued_status, std::future_status::ready);
  EXPECT_EQ(active.wait_for(0ms), std::future_status::timeout);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.submitted, 1U);
    EXPECT_EQ(state.ends, 0U);
  }
  // A ready result means Stage has unwound its mappings. Remove staging before
  // allowing the worker to process B's event; its pointer targets are gone.
  if (queued_status == std::future_status::ready) {
    std::error_code error;
    fs::remove_all(root_ / "queued", error);
    EXPECT_FALSE(error);
  }
  ReleaseEnd();
  EXPECT_NO_THROW(active.get());
  ExpectCancellation(queued, deadline ? CancellationError::Reason::DEADLINE_EXCEEDED :
                                       CancellationError::Reason::CANCELLED);
  EXPECT_NO_THROW(restore.Stage(Plan(), root_ / "recovered"));
  EXPECT_EQ(state.submitted, 2U);
  EXPECT_FALSE(restore.Failed());
}

INSTANTIATE_TEST_SUITE_P(CancellationCauses, QueuedTeardownCancellationTest, ::testing::Bool());

TEST_F(ModelStreamerSessionTest, CancellationDuringNativeRequestWaitsForTeardown)
{
  state.hold_request = true;
  state.hold_end = true;
  Cancellation control;
  ModelStreamerRestore restore;
  auto result = std::async(std::launch::async, [&] { restore.Stage(Plan(), root_ / "restore", control); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.requests_entered == 1; }));
    EXPECT_EQ(state.submitted, 0U);
  }
  control.Cancel();
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

TEST_F(ModelStreamerSessionTest, CoordinatorsWaitUntilPreviousNativeTeardownReturns)
{
  state.hold_end = true;
  ModelStreamerRestore first, second;
  auto active = std::async(std::launch::async, [&] { first.Stage(Plan(), root_ / "first"); });
  EXPECT_TRUE(WaitForEnd());
  auto waiting = std::async(std::launch::async, [&] { second.Stage(Plan(), root_ / "second"); });
  EXPECT_EQ(waiting.wait_for(50ms), std::future_status::timeout);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.starts, 1U);
  }
  ReleaseEnd();
  EXPECT_NO_THROW(active.get());
  EXPECT_NO_THROW(waiting.get());
  EXPECT_EQ(state.starts, 2U);
  EXPECT_EQ(state.ends, 2U);
}

TEST_F(ModelStreamerSessionTest, DeadlineWhileWaitingForAnotherCoordinatorIsIndependent)
{
  state.hold_end = true;
  ModelStreamerRestore first, second;
  auto active = std::async(std::launch::async, [&] { first.Stage(Plan(), root_ / "first"); });
  EXPECT_TRUE(WaitForEnd());
  Cancellation control;
  control = control.WithDeadline(Cancellation::Clock::now() + 50ms);
  auto waiting = std::async(std::launch::async, [&] { second.Stage(Plan(), root_ / "expired", control); });
  EXPECT_EQ(waiting.wait_for(2s), std::future_status::ready);
  EXPECT_EQ(active.wait_for(0ms), std::future_status::timeout);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.starts, 1U);
  }
  ReleaseEnd();
  EXPECT_NO_THROW(active.get());
  EXPECT_THROW(waiting.get(), CancellationError);
  EXPECT_FALSE(second.Failed());
  EXPECT_NO_THROW(second.Stage(Plan(), root_ / "recovered"));
}
