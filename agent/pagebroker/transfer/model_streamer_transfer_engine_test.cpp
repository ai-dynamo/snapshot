// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_transfer_engine.hpp"

#include <gtest/gtest.h>

#include <atomic>
#include <filesystem>
#include <fstream>
#include <iterator>
#include <map>
#include <stdexcept>
#include <string>
#include <vector>

#include "model_streamer_api.hpp"
#include "tests/temporary_directory.hpp"

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

namespace {
struct FakeResponse {
  model_streamer_api::SubmissionId submission_id;
  unsigned file_index;
  unsigned range_index;
  int submission_done;
};

struct FakeStreamer {
  bool fail_responses;
  std::vector<FakeResponse> responses;
  std::size_t next_response = 0;
};

std::atomic<unsigned> starts = 0;
std::atomic<unsigned> ends = 0;
std::atomic<model_streamer_api::SubmissionId> next_submission_id = 0;
bool fail_first_session = true;
int credential_status = 0;
unsigned credential_calls = 0;
std::map<std::string, std::string> configured;
}  // namespace

namespace snapshot::pagebroker::model_streamer_api {
extern "C" int
runai_start(void** streamer)
{
  const unsigned generation = ++starts;
  *streamer = new FakeStreamer{fail_first_session && generation == 1};
  return 0;
}

extern "C" int
runai_set_credentials(void* value, const char** keys, const char** values, unsigned count)
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
runai_end(void* streamer)
{
  ++ends;
  delete static_cast<FakeStreamer*>(streamer);
}

extern "C" int
runai_request(
    void* value,
    SubmissionId* out_submission_id,
    unsigned num_files,
    const char** paths,
    unsigned* num_ranges,
    std::size_t* range_offsets,
    std::size_t* range_sizes,
    void** range_destinations)
{
  auto& streamer = *static_cast<FakeStreamer*>(value);
  const SubmissionId submission_id = ++next_submission_id;
  *out_submission_id = submission_id;
  if (streamer.fail_responses) {
    streamer.responses.push_back(FakeResponse{submission_id + 1, 0, 0, 1});
    return 0;
  }

  std::size_t total_ranges = 0;
  for (unsigned file = 0; file < num_files; ++file)
    total_ranges += num_ranges[file];

  std::size_t range = 0;
  for (unsigned file = 0; file < num_files; ++file) {
    std::ifstream source(paths[file], std::ios::binary);
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
runai_response(
    void* value,
    SubmissionId* out_submission_id,
    unsigned* file_index,
    unsigned* range_index,
    int* submission_done,
    unsigned)
{
  auto& streamer = *static_cast<FakeStreamer*>(value);
  if (streamer.next_response == streamer.responses.size())
    return kTimedOutStatusCode;

  const auto& response = streamer.responses[streamer.next_response++];
  *out_submission_id = response.submission_id;
  *file_index = response.file_index;
  *range_index = response.range_index;
  *submission_done = response.submission_done;
  return 0;
}

extern "C" const char*
runai_response_str(int response_code)
{
  return response_code == kTimedOutStatusCode ? "timed out" : "fake Model Streamer error";
}
}  // namespace snapshot::pagebroker::model_streamer_api

TEST(ModelStreamerTransferEngineTest, ReplacesTerminallyFailedRestoreSession)
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
    EXPECT_THROW(engine.StageRestore(backend, root / "first"), std::runtime_error);
    EXPECT_EQ(starts, 1);
    EXPECT_EQ(ends, 1);

    ASSERT_NO_THROW(engine.StageRestore(backend, root / "second"));
    std::ifstream restored(root / "second" / "data");
    ASSERT_TRUE(restored);
    EXPECT_EQ(std::string(std::istreambuf_iterator<char>(restored), std::istreambuf_iterator<char>()), "recovered");
    EXPECT_EQ(starts, 2);
  }
  EXPECT_EQ(ends, 2);
  fs::remove_all(root);
}

TEST(ModelStreamerSessionTest, ConfiguresBeforeRequestsAndOnlyOnce)
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
  EXPECT_EQ(credential_calls, 1);
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
  restore.Stage({}, root.path() / "first");
  EXPECT_EQ(credential_calls, 1);
  EXPECT_EQ(configured.size(), 1);
  EXPECT_TRUE(configured.contains("region"));
  credential_calls = 0;
  ModelStreamerRestore defaults;
  defaults.Stage({}, root.path() / "default");
  EXPECT_EQ(credential_calls, 0);
}

TEST(ModelStreamerSessionTest, StopsSessionWhenConfigurationFailsAndCanRetryStart)
{
  fail_first_session = false;
  credential_status = 1;
  starts = 0;
  ends = 0;
  test::TemporaryDirectory root;
  ModelStreamerSessionOptions options;
  options.region = "test-region";
  {
    ModelStreamerRestore restore(options);
    EXPECT_THROW(restore.Stage({}, root.path() / "failed"), std::runtime_error);
    EXPECT_EQ(starts, 1);
    EXPECT_EQ(ends, 1);
    credential_status = 0;
    EXPECT_NO_THROW(restore.Stage({}, root.path() / "retry"));
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
