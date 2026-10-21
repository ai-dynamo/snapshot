// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_transfer_engine.hpp"

#include <gtest/gtest.h>
#include <sys/stat.h>
#include <unistd.h>

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <filesystem>
#include <fstream>
#include <future>
#include <iterator>
#include <map>
#include <mutex>
#include <stdexcept>
#include <string>
#include <vector>

#include <streamer/streamer.h>
#include "tests/temporary_directory.hpp"

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

namespace {
struct FakeResponse {
  RunaiFileStreamerSubmissionId submission_id;
  unsigned file_index;
  unsigned range_index;
  int submission_done;
};

struct FakeStreamer {
  bool fail_responses;
  bool stall_responses;
  std::vector<FakeResponse> responses = {};
  std::size_t next_response = 0;
  void* stalled_destination = nullptr;
};

std::atomic<unsigned> starts = 0;
std::atomic<unsigned> ends = 0;
std::atomic<RunaiFileStreamerSubmissionId> next_submission_id = 0;
bool fail_first_session = true;
bool timeout_first_session = false;
fs::path fake_s3_root;
int credential_status = 0;
unsigned credential_calls = 0;
std::mutex native_mutex;
std::condition_variable native_changed;
bool hold_end = false;
bool end_entered = false;
std::map<std::string, std::string> configured;

class ModelStreamerTransferEngineTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    starts = ends = next_submission_id = 0;
    fail_first_session = true;
    timeout_first_session = false;
    credential_status = credential_calls = 0;
    configured.clear();
    fake_s3_root.clear();
    hold_end = end_entered = false;
  }

  test::TemporaryDirectory root_;
};

}  // namespace

extern "C" int
runai_file_streamer_start(void** streamer)
{
  // Recovery may retain the old coordinator, but its native session must end
  // before a replacement starts.
  EXPECT_EQ(starts.load(), ends.load());
  const unsigned generation = ++starts;
  *streamer = new FakeStreamer{fail_first_session && generation == 1, timeout_first_session && generation == 1};
  return 0;
}

extern "C" int
runai_file_streamer_set_credentials(void* value, const char** keys, const char** values, unsigned count)
{
  EXPECT_NE(value, nullptr);
  EXPECT_TRUE(static_cast<FakeStreamer*>(value)->responses.empty());
  ++credential_calls;
  configured.clear();
  for (unsigned i = 0; i < count; ++i)
    configured.emplace(keys[i], values[i]);
  return credential_status;
}

extern "C" void
runai_file_streamer_end(void* streamer)
{
  std::unique_lock lock(native_mutex);
  if (hold_end) {
    end_entered = true;
    native_changed.notify_all();
    EXPECT_TRUE(native_changed.wait_for(lock, std::chrono::seconds(5), [] { return !hold_end; }));
    // Native access remains legal until runai_file_streamer_end returns.
    if (auto* destination = static_cast<FakeStreamer*>(streamer)->stalled_destination)
      *static_cast<char*>(destination) = 'x';
  }
  ++ends;
  delete static_cast<FakeStreamer*>(streamer);
}

extern "C" int
runai_file_streamer_request(
    void* value,
    RunaiFileStreamerSubmissionId* out_submission_id,
    unsigned num_files,
    const char** paths,
    unsigned* num_ranges,
    std::size_t* range_offsets,
    std::size_t* range_sizes,
    void** range_destinations,
    RunaiFileStreamerDevice device)
{
  EXPECT_EQ(device.type, RUNAI_FILE_STREAMER_DEVICE_CPU);
  EXPECT_EQ(device.id, 0);
  auto& streamer = *static_cast<FakeStreamer*>(value);
  RunaiFileStreamerSubmissionId submission_id;
  {
    std::lock_guard lock(native_mutex);
    submission_id = ++next_submission_id;
    native_changed.notify_all();
  }
  *out_submission_id = submission_id;
  if (streamer.stall_responses) {
    streamer.stalled_destination = range_destinations[0];
    return 0;
  }
  if (streamer.fail_responses) {
    streamer.responses.push_back(FakeResponse{submission_id + 1, 0, 0, 1});
    return 0;
  }

  std::size_t total_ranges = 0;
  for (unsigned file = 0; file < num_files; ++file)
    total_ranges += num_ranges[file];

  std::size_t range = 0;
  for (unsigned file = 0; file < num_files; ++file) {
    const std::string path(paths[file]);
    const std::string prefix = "s3://local-test-bucket/";
    if (!path.starts_with(prefix))
      return 1;
    std::ifstream source(fake_s3_root / path.substr(prefix.size()), std::ios::binary);
    if (!source)
      return 1;
    for (unsigned file_range = 0; file_range < num_ranges[file]; ++file_range, ++range) {
      source.seekg(static_cast<std::streamoff>(range_offsets[range]));
      source.read(static_cast<char*>(range_destinations[range]), static_cast<std::streamsize>(range_sizes[range]));
      if (source.gcount() != static_cast<std::streamsize>(range_sizes[range]))
        return 1;
      streamer.responses.push_back(
          FakeResponse{submission_id, file, file_range, range + 1 == total_ranges});
    }
  }
  return 0;
}

extern "C" int
runai_file_streamer_response(
    void* value,
    RunaiFileStreamerSubmissionId* out_submission_id,
    unsigned* file_index,
    unsigned* range_index,
    int* submission_done,
    unsigned)
{
  auto& streamer = *static_cast<FakeStreamer*>(value);
  if (streamer.next_response == streamer.responses.size())
    return RUNAI_FILE_STREAMER_RESPONSE_TIMED_OUT;

  const auto& response = streamer.responses[streamer.next_response++];
  *out_submission_id = response.submission_id;
  *file_index = response.file_index;
  *range_index = response.range_index;
  *submission_done = response.submission_done;
  return 0;
}

extern "C" const char*
runai_file_streamer_response_str(int response_code)
{
  return response_code == RUNAI_FILE_STREAMER_RESPONSE_TIMED_OUT ? "timed out" : "fake Model Streamer error";
}


TEST_F(ModelStreamerTransferEngineTest, RejectsDirectAndBorrowedRestoreBeforeNativeStartup)
{
  ModelStreamerTransferEngine engine;
  for (int kind = 0; kind < 3; ++kind) {
    PreparedRestore prepared;
    prepared.inventory.files.push_back({"s3://local-test-bucket/source", "data", 1});
    if (kind == 0)
      prepared.mode = TransferMode::Direct;
    else if (kind == 1)
      prepared.local_source = root_.path();
    else
      prepared.source = FileDescriptor::Duplicate(STDIN_FILENO);
    EXPECT_THROW(engine.StageRestore(std::move(prepared), root_.path() / "unused"), std::invalid_argument);
  }
  EXPECT_FALSE(fs::exists(root_.path() / "unused"));
  EXPECT_EQ(starts, 0U);
}

TEST_F(ModelStreamerTransferEngineTest, RejectsUnsupportedStorageOperations)
{
  ModelStreamerTransferEngine engine;
  StorageBackend storage;
  storage.mutable_filesystem()->set_directory((root_.path() / "unused").string());
  EXPECT_THROW(engine.PrepareRestore(storage), std::logic_error);
  EXPECT_THROW(engine.PrepareCheckpoint(storage, root_.path() / "capture", "id"), std::logic_error);
  EXPECT_THROW(engine.ValidateCheckpointDestination(storage), std::logic_error);
  EXPECT_THROW(engine.InspectCheckpoint(root_.path()), std::logic_error);
  EXPECT_THROW(engine.PublishCheckpoint(root_.path(), storage, {}), std::logic_error);
  EXPECT_TRUE(fs::is_empty(root_.path()));
  EXPECT_EQ(starts, 0U);
}

TEST_F(ModelStreamerTransferEngineTest, ReplacesTerminallyFailedRestoreSession)
{
  starts = 0;
  ends = 0;
  next_submission_id = 0;
  fail_first_session = true;

  const fs::path root = root_.path();
  const fs::path storage = root / "storage";
  const fs::path source = storage / "source";
  fs::create_directories(source);
  std::ofstream(source / "data") << "recovered";

  fake_s3_root = source;
  const auto prepare = [] {
    PreparedRestore result;
    result.inventory.files.push_back({"s3://local-test-bucket/data", "data", 9});
    return result;
  };
  {
    ModelStreamerTransferEngine engine;
    EXPECT_THROW(engine.StageRestore(prepare(), root / "first"), std::runtime_error);
    EXPECT_EQ(starts, 1);
    EXPECT_EQ(ends, 1);

    ASSERT_NO_THROW(engine.StageRestore(prepare(), root / "second"));
    std::ifstream restored(root / "second" / "data");
    ASSERT_TRUE(restored);
    EXPECT_EQ(std::string(std::istreambuf_iterator<char>(restored), std::istreambuf_iterator<char>()), "recovered");
    EXPECT_EQ(starts, 2);
  }
  EXPECT_EQ(ends, 2);
}

TEST_F(ModelStreamerTransferEngineTest, ConfiguresBeforeRequestsInEveryNativeSession)
{
  fail_first_session = false;
  credential_status = 0;
  credential_calls = 0;
  test::TemporaryDirectory root;
  ModelStreamerSessionOptions options;
  options.region = "test-region";
  options.endpoint = "http://s3.example.invalid";
  options.access_key_id = "fake-access-key";
  options.secret_access_key = "fake-secret-key";
  options.session_token = "fake-session-token";
  ModelStreamerRestore restore(options);
  // Options are owned by the session, independent of the caller's storage.
  options.region = "changed";
  CheckpointInventory inventory;
  const auto source = root.path() / "source";
  std::ofstream(source) << "data";
  fake_s3_root = root.path();
  inventory.files.push_back({"s3://local-test-bucket/source", "data", 4});
  restore.Stage(inventory, root.path() / "first");
  restore.Stage(inventory, root.path() / "second");
  EXPECT_EQ(credential_calls, 2);
  EXPECT_EQ(configured.size(), 5);
  EXPECT_EQ(configured.at("region"), "test-region");
  EXPECT_EQ(configured.at("endpoint"), options.endpoint);
  EXPECT_EQ(configured.at("access_key_id"), options.access_key_id);
  EXPECT_EQ(configured.at("secret_access_key"), options.secret_access_key);
  EXPECT_EQ(configured.at("session_token"), options.session_token);
}

TEST_F(ModelStreamerTransferEngineTest, PreservesProviderChainWithConnectionOnlyOptions)
{
  fail_first_session = false;
  credential_calls = 0;
  test::TemporaryDirectory root;
  ModelStreamerSessionOptions options;
  options.region = "test-region";
  CheckpointInventory inventory;
  const auto source = root.path() / "source";
  std::ofstream(source) << "data";
  fake_s3_root = root.path();
  inventory.files.push_back({"s3://local-test-bucket/source", "data", 4});
  {
    ModelStreamerRestore restore(options);
    restore.Stage(inventory, root.path() / "first");
    EXPECT_EQ(credential_calls, 1);
    EXPECT_EQ(configured.size(), 1);
    EXPECT_TRUE(configured.contains("region"));
  }  // Stage may return before teardown; destroy this coordinator before starting another.
  credential_calls = 0;
  ModelStreamerRestore defaults;
  defaults.Stage(inventory, root.path() / "default");
  EXPECT_EQ(credential_calls, 0);
}

TEST_F(ModelStreamerTransferEngineTest, StopsSessionWhenConfigurationFails)
{
  fail_first_session = false;
  credential_status = 1;
  starts = 0;
  ends = 0;
  test::TemporaryDirectory root;
  ModelStreamerSessionOptions options;
  options.region = "test-region";
  CheckpointInventory inventory;
  const auto source = root.path() / "source";
  std::ofstream(source) << "data";
  fake_s3_root = root.path();
  inventory.files.push_back({"s3://local-test-bucket/source", "data", 4});
  {
    ModelStreamerRestore restore(options);
    EXPECT_THROW(restore.Stage(inventory, root.path() / "failed"), std::runtime_error);
    EXPECT_EQ(starts, 1);
    EXPECT_EQ(ends, 1);
    EXPECT_TRUE(restore.Failed());
    credential_status = 0;
    ModelStreamerRestore replacement(options);
    EXPECT_NO_THROW(replacement.Stage(inventory, root.path() / "retry"));
    EXPECT_EQ(starts, 2);
  }
  EXPECT_EQ(ends, 2);
}

TEST_F(ModelStreamerTransferEngineTest, RejectsIncompleteCredentialsAndEmbeddedNul)
{
  ModelStreamerSessionOptions options;
  options.access_key_id = "fake-access-key";
  EXPECT_THROW({ ModelStreamerRestore restore(options); }, std::invalid_argument);
  options.access_key_id.clear();
  options.secret_access_key = "fake-secret-key";
  EXPECT_THROW({ ModelStreamerRestore restore(options); }, std::invalid_argument);
  options.secret_access_key.clear();
  options.session_token = "fake-session-token";
  EXPECT_THROW({ ModelStreamerRestore restore(options); }, std::invalid_argument);
  options.session_token.clear();
  options.endpoint = std::string("http://host\0suffix", 18);
  EXPECT_THROW({ ModelStreamerRestore restore(options); }, std::invalid_argument);
}

TEST_F(ModelStreamerTransferEngineTest, CancelledRestoreDrainsBeforeEngineReplacesCoordinator)
{
  fail_first_session = false;
  timeout_first_session = true;
  hold_end = true;
  fake_s3_root = root_.path();
  std::ofstream(root_.path() / "source") << "a";
  CheckpointInventory inventory;
  inventory.files.push_back({"s3://local-test-bucket/source", "data", 1, fs::perms::owner_read});
  ModelStreamerTransferEngine engine;
  const auto prepare = [&] {
    PreparedRestore prepared;
    prepared.inventory = inventory;
    return prepared;
  };
  Cancellation cancellation;
  auto stage = std::async(std::launch::async, [&] {
    engine.StageRestore(prepare(), root_.path() / "cancelled",
        cancellation.WithDeadline(Cancellation::Clock::now() + std::chrono::seconds(5)));
  });
  {
    std::unique_lock lock(native_mutex);
    EXPECT_TRUE(native_changed.wait_for(lock, std::chrono::seconds(2), [] { return next_submission_id != 0; }));
  }
  cancellation.Cancel();
  {
    std::unique_lock lock(native_mutex);
    EXPECT_TRUE(native_changed.wait_for(lock, std::chrono::seconds(2), [] { return end_entered; }));
  }
  EXPECT_EQ(stage.wait_for(std::chrono::milliseconds(50)), std::future_status::timeout);
  EXPECT_THROW(engine.StageRestore(prepare(), root_.path() / "during-teardown"), std::runtime_error);
  EXPECT_EQ(starts, 1U);
  EXPECT_EQ(ends, 0U);
  {
    std::lock_guard lock(native_mutex);
    hold_end = false;
    native_changed.notify_all();
  }
  EXPECT_THROW(stage.get(), CancellationError);
  EXPECT_EQ(ends, 1U);
  char byte = 0;
  std::ifstream(root_.path() / "cancelled/data").get(byte);
  EXPECT_EQ(byte, 'x');  // Teardown could still write before the cancelled caller returned.
  EXPECT_NO_THROW(engine.StageRestore(prepare(), root_.path() / "recovered"));
  EXPECT_EQ(starts, 2U);
  std::ifstream(root_.path() / "recovered/data").get(byte);
  EXPECT_EQ(byte, 'a');
}
