// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"

#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <future>
#include <mutex>
#include <string>
#include <vector>

#include "transfer/engine/model_streamer/model_streamer_api.hpp"

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
  bool allow_responses = true;
  unsigned starts = 0;
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
    state.allow_responses = true;
    state.starts = state.ends_entered = state.ends = state.submitted = state.max_submissions = 0;
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
  Path root_;
};
}  // namespace

extern "C" int runai_file_streamer_start(void** value)
{
  *value = new Session;
  std::unique_lock lock(state.mutex);
  ++state.starts;
  state.changed.notify_all();
  state.changed.wait(lock, [] { return !state.hold_start; });
  return RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
}

extern "C" int runai_file_streamer_set_credentials(void*, const char**, const char**, unsigned)
{
  return RUNAI_FILE_STREAMER_RESPONSE_SUCCESS;
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
  std::lock_guard lock(state.mutex);
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
  state.allow_responses = false;
  ModelStreamerRestore restore;
  auto plan = Plan();
  plan.files.front().size_bytes = 6'000'000'000;
  auto active = std::async(std::launch::async, [&] { restore.Stage(plan, root_ / "active"); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted == 1; }));
  }
  TransferControl control;
  control.deadline = TransferControl::Clock::now() + 50ms;
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
  EXPECT_THROW(queued.get(), TransferInterrupted);
  EXPECT_NO_THROW(active.get());
  EXPECT_FALSE(restore.Failed());
}

TEST_F(ModelStreamerSessionTest, ActiveCancellationWaitsForNativeTeardown)
{
  state.allow_responses = false;
  state.hold_end = true;
  std::stop_source stop;
  ModelStreamerRestore restore;
  TransferControl control;
  control.cancellation = stop.get_token();
  auto result = std::async(std::launch::async, [&] { restore.Stage(Plan(), root_ / "restore", control); });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.submitted == 1; }));
  }
  stop.request_stop();
  EXPECT_TRUE(WaitForEnd());
  EXPECT_EQ(result.wait_for(0ms), std::future_status::timeout);
  ReleaseEnd();
  EXPECT_THROW(result.get(), TransferInterrupted);
  EXPECT_TRUE(restore.Failed());
}

TEST_F(ModelStreamerSessionTest, CancellationDuringStartupDoesNotFailCoordinator)
{
  state.hold_start = true;
  std::stop_source stop;
  ModelStreamerRestore restore;
  TransferControl control;
  control.cancellation = stop.get_token();
  auto cancelled = std::async(std::launch::async, [&] {
    restore.Stage(Plan(), root_ / "cancelled", control);
  });
  {
    std::unique_lock lock(state.mutex);
    EXPECT_TRUE(state.changed.wait_for(lock, 5s, [] { return state.starts == 1; }));
    stop.request_stop();
    state.hold_start = false;
    state.changed.notify_all();
  }
  EXPECT_THROW(cancelled.get(), TransferInterrupted);
  EXPECT_EQ(state.submitted, 0U);
  EXPECT_FALSE(restore.Failed());
  EXPECT_NO_THROW(restore.Stage(Plan(), root_ / "success"));
  EXPECT_EQ(state.submitted, 1U);
  EXPECT_EQ(state.starts, state.ends);
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
  TransferControl control;
  control.deadline = TransferControl::Clock::now() + 50ms;
  auto waiting = std::async(std::launch::async, [&] { second.Stage(Plan(), root_ / "expired", control); });
  EXPECT_EQ(waiting.wait_for(2s), std::future_status::ready);
  EXPECT_EQ(active.wait_for(0ms), std::future_status::timeout);
  {
    std::lock_guard lock(state.mutex);
    EXPECT_EQ(state.starts, 1U);
  }
  ReleaseEnd();
  EXPECT_NO_THROW(active.get());
  EXPECT_THROW(waiting.get(), TransferInterrupted);
  EXPECT_FALSE(second.Failed());
  EXPECT_NO_THROW(second.Stage(Plan(), root_ / "recovered"));
}
