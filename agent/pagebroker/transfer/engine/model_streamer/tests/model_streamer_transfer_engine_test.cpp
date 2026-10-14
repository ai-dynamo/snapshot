// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_transfer_engine.hpp"

#include <gtest/gtest.h>
#include <sys/stat.h>

#include <atomic>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <future>
#include <iterator>
#include <map>
#include <optional>
#include <stdexcept>
#include <string>
#include <thread>
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
std::atomic<bool> hold_end = false;
std::atomic<bool> end_entered = false;
std::atomic<bool> release_end = false;
std::map<std::string, std::string> configured;

class ScopedEnvironment {
 public:
  ScopedEnvironment(const char* name, const char* value) : name_(name)
  {
    if (const auto* previous = std::getenv(name))
      previous_ = previous;
    if (value)
      setenv(name, value, 1);
    else
      unsetenv(name);
  }
  ~ScopedEnvironment()
  {
    if (previous_)
      setenv(name_, previous_->c_str(), 1);
    else
      unsetenv(name_);
  }

 private:
  const char* name_;
  std::optional<std::string> previous_;
};

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

  }

  ScopedEnvironment addressing_{"RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING", "0"};
  ScopedEnvironment ca_{"AWS_CA_BUNDLE", nullptr};
  test::TemporaryDirectory root_;
  std::chrono::milliseconds restore_timeout_{50};
};

}  // namespace

extern "C" int
runai_file_streamer_start(void** streamer)
{
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
  if (hold_end) {
    end_entered = true;
    while (!release_end)
      std::this_thread::yield();
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
  const RunaiFileStreamerSubmissionId submission_id = ++next_submission_id;
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
    std::ifstream source(path.starts_with(prefix) ? fake_s3_root / path.substr(prefix.size()) : fs::path(path),
                         std::ios::binary);
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


TEST_F(ModelStreamerTransferEngineTest, DirectRestoreBorrowsSourceWithoutStartingNativeSession)
{
  const auto source = root_.path() / "source";
  fs::create_directory(source);
  ASSERT_EQ(mkfifo((source / "unscanned").c_str(), 0600), 0);
  StorageBackend backend;
  backend.mutable_filesystem()->set_directory(source.string());
  ModelStreamerTransferEngine engine(root_.path());
  auto prepared = engine.PrepareRestore(backend, {}, nullptr, false, TransferMode::Direct);
  EXPECT_EQ(prepared.plan.size_bytes(), 0u);
  const auto access = engine.StageRestore(std::move(prepared), root_.path() / "unused");
  EXPECT_EQ(access.image_directory, source);
  EXPECT_TRUE(access.staging_directory.empty());
  EXPECT_GE(access.source.get(), 0);
  EXPECT_FALSE(fs::exists(root_.path() / "unused"));
  EXPECT_EQ(starts, 0u);
}

TEST_F(ModelStreamerTransferEngineTest, PublishesBothFilesystemModesWithoutStartingNativeSession)
{
  ModelStreamerTransferEngine engine(root_.path());
  for (const auto mode : {TransferMode::Direct, TransferMode::Staged}) {
    StorageBackend backend;
    const auto destination = root_.path() / (mode == TransferMode::Direct ? "direct" : "staged");
    backend.mutable_filesystem()->set_directory(destination.string());
    const auto output = engine.PrepareCheckpoint(backend, root_.path() / "staging", "checkpoint", mode);
    fs::create_directory(output);
    std::ofstream(output / "data") << "checkpoint";
    struct stat before{}, after{};
    ASSERT_EQ(stat((output / "data").c_str(), &before), 0);
    engine.PublishCheckpoint(output, backend, engine.InspectCheckpoint(output), nullptr, {}, mode);
    ASSERT_EQ(stat((destination / "data").c_str(), &after), 0);
    if (mode == TransferMode::Direct) {
      EXPECT_EQ(before.st_ino, after.st_ino);
      EXPECT_FALSE(fs::exists(output));
    } else {
      EXPECT_NE(before.st_ino, after.st_ino);
      EXPECT_TRUE(fs::exists(output / "data"));
    }
  }
  EXPECT_EQ(starts, 0u);
}

TEST_F(ModelStreamerTransferEngineTest, ReplacesTerminallyFailedRestoreSession)
{
  starts = 0;
  ends = 0;
  next_submission_id = 0;
  fail_first_session = true;

  const fs::path root = fs::temp_directory_path() / "pagebroker-model-streamer-recovery-test";
  fs::remove_all(root);
  const fs::path storage = root / "storage";
  const fs::path source = storage / "source";
  fs::create_directories(source);
  std::ofstream(source / "data") << "recovered";

  StorageBackend backend;
  backend.mutable_filesystem()->set_directory(source.string());
  {
    ModelStreamerTransferEngine engine(storage);
    EXPECT_THROW(engine.StageRestore(engine.PrepareRestore(backend), root / "first"), std::runtime_error);
    EXPECT_EQ(starts, 1);
    EXPECT_EQ(ends, 1);

    ASSERT_NO_THROW(engine.StageRestore(engine.PrepareRestore(backend), root / "second"));
    std::ifstream restored(root / "second" / "data");
    ASSERT_TRUE(restored);
    EXPECT_EQ(std::string(std::istreambuf_iterator<char>(restored), std::istreambuf_iterator<char>()), "recovered");
    EXPECT_EQ(starts, 2);
  }
  EXPECT_EQ(ends, 2);
  fs::remove_all(root);
}

TEST_F(ModelStreamerTransferEngineTest, RejectsMissingStorageBeforeCreatingAnything)
{
  ModelStreamerTransferEngine engine(root_.path());
  EXPECT_THROW(engine.PrepareRestore({}), std::invalid_argument);
  EXPECT_THROW(engine.StageRestore(engine.PrepareRestore({}), root_.path() / "staged"), std::invalid_argument);
  EXPECT_FALSE(fs::exists(root_.path() / "staged"));
  EXPECT_EQ(starts, 0);
}







TEST(ModelStreamerSessionTest, ConfiguresBeforeRequestsInEveryNativeSession)
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
  RestorePlan plan;
  const auto source = root.path() / "source";
  std::ofstream(source) << "data";
  plan.files.push_back({source.string(), "data", 4});
  restore.Stage(plan, root.path() / "first");
  restore.Stage(plan, root.path() / "second");
  EXPECT_EQ(credential_calls, 2);
  EXPECT_EQ(configured.size(), 5);
  EXPECT_EQ(configured.at("region"), "test-region");
  EXPECT_EQ(configured.at("endpoint"), options.endpoint);
  EXPECT_EQ(configured.at("access_key_id"), options.access_key_id);
  EXPECT_EQ(configured.at("secret_access_key"), options.secret_access_key);
  EXPECT_EQ(configured.at("session_token"), options.session_token);
}

TEST(ModelStreamerSessionTest, PreservesProviderChainWithConnectionOnlyOptions)
{
  fail_first_session = false;
  credential_calls = 0;
  test::TemporaryDirectory root;
  ModelStreamerSessionOptions options;
  options.region = "test-region";
  ModelStreamerRestore restore(options);
  RestorePlan plan;
  const auto source = root.path() / "source";
  std::ofstream(source) << "data";
  plan.files.push_back({source.string(), "data", 4});
  restore.Stage(plan, root.path() / "first");
  EXPECT_EQ(credential_calls, 1);
  EXPECT_EQ(configured.size(), 1);
  EXPECT_TRUE(configured.contains("region"));
  credential_calls = 0;
  ModelStreamerRestore defaults;
  defaults.Stage(plan, root.path() / "default");
  EXPECT_EQ(credential_calls, 0);
}

TEST(ModelStreamerSessionTest, StopsSessionWhenConfigurationFails)
{
  fail_first_session = false;
  credential_status = 1;
  starts = 0;
  ends = 0;
  test::TemporaryDirectory root;
  ModelStreamerSessionOptions options;
  options.region = "test-region";
  RestorePlan plan;
  const auto source = root.path() / "source";
  std::ofstream(source) << "data";
  plan.files.push_back({source.string(), "data", 4});
  {
    ModelStreamerRestore restore(options);
    EXPECT_THROW(restore.Stage(plan, root.path() / "failed"), std::runtime_error);
    EXPECT_EQ(starts, 1);
    EXPECT_EQ(ends, 1);
    EXPECT_TRUE(restore.Failed());
    credential_status = 0;
    ModelStreamerRestore replacement(options);
    EXPECT_NO_THROW(replacement.Stage(plan, root.path() / "retry"));
    EXPECT_EQ(starts, 2);
  }
  EXPECT_EQ(ends, 2);
}

TEST(ModelStreamerSessionTest, RejectsIncompleteCredentialsAndEmbeddedNul)
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

TEST_F(ModelStreamerTransferEngineTest, CancellationKeepsMappingsAliveUntilNativeDrain)
{
  fail_first_session = false;
  timeout_first_session = true;
  end_entered = release_end = false;
  hold_end = true;
  restore_timeout_ = std::chrono::hours(1);
  fake_s3_root = root_.path();
  std::ofstream(root_.path() / "source") << "a";
  RestorePlan plan;
  plan.files.push_back({"s3://local-test-bucket/source", "data", 1, fs::perms::owner_read});
  ModelStreamerRestore restore(restore_timeout_);
  std::stop_source cancellation;
  auto stage = std::async(std::launch::async, [&] {
    restore.Stage(plan, root_.path() / "cancelled", {TransferControl::Clock::now() + std::chrono::seconds(5), cancellation.get_token()});
  });
  const auto wait_until = TransferControl::Clock::now() + std::chrono::seconds(2);
  while (next_submission_id == 0 && TransferControl::Clock::now() < wait_until)
    std::this_thread::yield();
  EXPECT_GT(next_submission_id, 0U);
  cancellation.request_stop();
  while (!end_entered && TransferControl::Clock::now() < wait_until)
    std::this_thread::yield();
  EXPECT_TRUE(end_entered);
  EXPECT_EQ(stage.wait_for(std::chrono::milliseconds(50)), std::future_status::timeout);
  release_end = true;
  EXPECT_THROW(stage.get(), TransferInterrupted);
  hold_end = false;
  EXPECT_EQ(ends, 1U);
  EXPECT_TRUE(restore.Failed());
  ModelStreamerRestore recovered;
  EXPECT_NO_THROW(recovered.Stage(plan, root_.path() / "recovered"));
}
