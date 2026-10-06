// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_config.hpp"

#include <gtest/gtest.h>

#include <cstdlib>
#include <fstream>
#include <map>

#include "tests/temporary_directory.hpp"
#include "transfer/engine/model_streamer/model_streamer_transfer_engine.hpp"

namespace snapshot::pagebroker {
namespace {
class ModelStreamerConfigTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    for (const auto* suffix : {"FS_STRATEGY", "FS_QUEUE_DEPTH", "FS_CHUNK_BYTESIZE", "FS_MAX_ENGINES",
        "CHUNK_BYTESIZE", "PROCESS_GROUP_SIZE", "OBJ_CONCURRENCY", "CONCURRENCY", "S3_TARGET_GBPS",
        "S3_MAX_CONNECTIONS", "S3_MAX_INFLIGHT_MIB", "S3_MAX_RETRIES", "S3_TIMEOUT", "S3_REQUEST_TIMEOUT_MS",
        "S3_LOW_SPEED_LIMIT", "LOG_LEVEL", "LOG_TO_STDERR", "S3_USE_VIRTUAL_ADDRESSING"})
      SaveAndClear(std::string("RUNAI_STREAMER_") + suffix);
    SaveAndClear("AWS_CA_BUNDLE");
    SaveAndClear("AWS_EC2_METADATA_DISABLED");
  }
  void TearDown() override
  {
    for (const auto& [name, value] : previous_) {
      if (value)
        setenv(name.c_str(), value->c_str(), 1);
      else
        unsetenv(name.c_str());
    }
  }
  void SaveAndClear(const std::string& name)
  {
    const auto* value = std::getenv(name.c_str());
    previous_[name] = value ? std::optional<std::string>(value) : std::nullopt;
    unsetenv(name.c_str());
  }
  std::map<std::string, std::optional<std::string>> previous_;
};

TEST_F(ModelStreamerConfigTest, OmissionPreservesLibraryAndLegacyDaemonDefaults)
{
  auto options = ResolveModelStreamerOptions(ParseModelStreamerConfig("{}"));
  EXPECT_TRUE(options.NativeEnvironment().empty());
  S3Config storage;
  options = ResolveModelStreamerOptions({}, &storage);
  const auto env = options.NativeEnvironment();
  EXPECT_EQ(env.at("RUNAI_STREAMER_S3_MAX_RETRIES"), "3");
  EXPECT_EQ(env.at("RUNAI_STREAMER_S3_TIMEOUT"), "0");
  EXPECT_EQ(env.at("RUNAI_STREAMER_S3_REQUEST_TIMEOUT_MS"), "60000");
  EXPECT_EQ(env.at("RUNAI_STREAMER_S3_MAX_INFLIGHT_MIB"), "128");
  EXPECT_EQ(env.at("RUNAI_STREAMER_CHUNK_BYTESIZE"), "16777216");
  EXPECT_EQ(env.at("RUNAI_STREAMER_S3_MAX_CONNECTIONS"), "4");
  EXPECT_FALSE(env.contains("RUNAI_STREAMER_FS_QUEUE_DEPTH"));
}

TEST_F(ModelStreamerConfigTest, ResolvesJsonBeforeEnvironmentBeforeUploadFallback)
{
  setenv("RUNAI_STREAMER_S3_MAX_CONNECTIONS", "invalid-but-overridden", 1);
  setenv("RUNAI_STREAMER_S3_MAX_INFLIGHT_MIB", "32", 1);
  setenv("RUNAI_STREAMER_S3_MAX_RETRIES", "0", 1);
  setenv("RUNAI_STREAMER_CONCURRENCY", "6", 1);
  setenv("RUNAI_STREAMER_OBJ_CONCURRENCY", "3", 1);
  S3Config storage;
  auto options = ResolveModelStreamerOptions(ParseModelStreamerConfig(R"({
    "filesystem":{"strategies":["io_uring_buffered","sync_buffered"],"queueDepth":"64,nfs=16"},
    "s3Reader":{"maxConnections":8},"logging":{"toStderr":false}
  })"), &storage);
  EXPECT_EQ(options.s3_max_connections, 8);
  EXPECT_EQ(options.s3_max_inflight_mib, 32);
  EXPECT_EQ(options.s3_max_retries, 0);
  EXPECT_EQ(options.s3_concurrency, 3);
  EXPECT_EQ(options.filesystem_queue_depth, "64,nfs=16");
  EXPECT_EQ(storage.transfer.upload_limits.workers, 4);
  EXPECT_EQ(storage.transfer.upload_limits.buffer_budget, 128 * 1024 * 1024);
  EXPECT_EQ(std::string(std::getenv("RUNAI_STREAMER_S3_MAX_CONNECTIONS")), "invalid-but-overridden");
  ConfigureModelStreamerEnvironment(options, &storage);
  EXPECT_NO_THROW(options.ValidateEnvironment());
  EXPECT_STREQ(std::getenv("RUNAI_STREAMER_S3_MAX_CONNECTIONS"), "8");
  EXPECT_STREQ(std::getenv("RUNAI_STREAMER_LOG_TO_STDERR"), "0");
  EXPECT_STREQ(std::getenv("AWS_CA_BUNDLE"), "");
}

TEST_F(ModelStreamerConfigTest, LegacyConcurrencySuppliesBothPools)
{
  setenv("RUNAI_STREAMER_CONCURRENCY", "4", 1);
  const auto options = ResolveModelStreamerOptions({});
  EXPECT_EQ(options.filesystem_queue_depth, "4");
  EXPECT_EQ(options.s3_concurrency, 4);
}

TEST_F(ModelStreamerConfigTest, AppliesAllNativeSettingsAndSharedConnection)
{
  const auto options = ParseModelStreamerConfig(R"({
    "filesystem":{"strategies":["libaio_direct","sync_buffered"],"queueDepth":"32,nfs=8",
                  "chunkBytes":1048576,"maxEngines":2},
    "readChunkBytes":8388608,"processGroupSize":2,
    "s3Reader":{"concurrency":2,"targetGbps":10,"maxConnections":8,"maxInflightMiB":64,
                "maxRetries":0,"retryWindowSeconds":0,"lowSpeedTimeoutMs":2000,"lowSpeedBytesPerSecond":1024},
    "logging":{"level":"DEBUG","toStderr":true}
  })");
  S3Config storage;
  storage.transfer.connection.use_virtual_addressing = false;
  storage.transfer.connection.ca_file = "/test/ca.pem";
  ConfigureModelStreamerEnvironment(options, &storage);
  const std::map<std::string, std::string> expected{
      {"FS_STRATEGY", "libaio_direct,sync_buffered"}, {"FS_QUEUE_DEPTH", "32,nfs=8"},
      {"FS_CHUNK_BYTESIZE", "1048576"}, {"FS_MAX_ENGINES", "2"}, {"CHUNK_BYTESIZE", "8388608"},
      {"PROCESS_GROUP_SIZE", "2"}, {"OBJ_CONCURRENCY", "2"}, {"S3_TARGET_GBPS", "10"},
      {"S3_MAX_CONNECTIONS", "8"}, {"S3_MAX_INFLIGHT_MIB", "64"}, {"S3_MAX_RETRIES", "0"},
      {"S3_TIMEOUT", "0"}, {"S3_REQUEST_TIMEOUT_MS", "2000"}, {"S3_LOW_SPEED_LIMIT", "1024"},
      {"LOG_LEVEL", "DEBUG"}, {"LOG_TO_STDERR", "1"}, {"S3_USE_VIRTUAL_ADDRESSING", "0"}};
  for (const auto& [suffix, value] : expected)
    EXPECT_STREQ(std::getenv(("RUNAI_STREAMER_" + suffix).c_str()), value.c_str()) << suffix;
  EXPECT_STREQ(std::getenv("AWS_CA_BUNDLE"), "/test/ca.pem");
  EXPECT_STREQ(std::getenv("AWS_EC2_METADATA_DISABLED"), "true");
  EXPECT_NO_THROW(options.ValidateEnvironment());
  setenv("RUNAI_STREAMER_OBJ_CONCURRENCY", "7", 1);
  EXPECT_THROW(options.ValidateEnvironment(), std::invalid_argument);
}

TEST_F(ModelStreamerConfigTest, RejectsMalformedConfigurationWithoutEchoingValues)
{
  for (const auto* text : {
      "[]", "null", "{", R"({"unknown-secret":"do-not-print"})", R"({"readChunkBytes":1,"readChunkBytes":2})",
      R"({"filesystem":[]})", R"({"filesystem":{"strategies":[]}})",
      R"({"filesystem":{"strategies":[""]}})", R"({"filesystem":{"strategies":["sync_buffered",""]}})",
      R"({"filesystem":{"strategies":["sync_buffered","sync_buffered"]}})",
      R"({"filesystem":{"strategies":["unknown"]}})", R"({"filesystem":{"strategies":["io_uring_direct,sync_buffered"]}})",
      R"({"filesystem":{"queueDepth":"0"}})", R"({"filesystem":{"queueDepth":"4,nfs=0"}})",
      R"({"filesystem":{"queueDepth":"4,NFS=2,nfs=3"}})", R"({"filesystem":{"queueDepth":"4,"}})",
      R"({"filesystem":{"queueDepth":"4294967296"}})", R"({"filesystem":{"queueDepth":4}})",
      R"({"filesystem":{"maxEngines":1025}})", R"({"filesystem":{"chunkBytes":0}})",
      R"({"s3Reader":{"concurrency":-1}})", R"({"s3Reader":{"concurrency":1.0}})",
      R"({"s3Reader":{"concurrency":"2"}})", R"({"s3Reader":{"concurrency":1025}})",
      R"({"s3Reader":{"maxConnections":18446744073709551616}})",
      R"({"logging":{"level":"do-not-print"}})", R"({"logging":{"toStderr":1}})",
      R"({"filesystem":{"queueDepth":"4\u0000"}})"}) {
    SCOPED_TRACE(text);
    try {
      ParseModelStreamerConfig(text);
      FAIL() << "accepted invalid configuration";
    }
    catch (const std::invalid_argument& error) {
      EXPECT_EQ(std::string(error.what()).find("do-not-print"), std::string::npos);
    }
  }
  EXPECT_THROW(ParseModelStreamerConfig(std::string(65537, ' ')), std::invalid_argument);
}

TEST_F(ModelStreamerConfigTest, InvalidEnvironmentFailsBeforeAnySettingsAreApplied)
{
  setenv("RUNAI_STREAMER_S3_MAX_CONNECTIONS", "bad", 1);
  S3Config storage;
  EXPECT_THROW(ResolveModelStreamerOptions({}, &storage), std::invalid_argument);
  EXPECT_EQ(std::getenv("RUNAI_STREAMER_S3_MAX_RETRIES"), nullptr);
  EXPECT_EQ(std::getenv("AWS_CA_BUNDLE"), nullptr);
}

TEST_F(ModelStreamerConfigTest, RestoresFilesystemWithConfiguredNativeOptions)
{
  test::TemporaryDirectory root;
  const auto source = root.path() / "source";
  std::filesystem::create_directory(source);
  const std::string contents(65539, 'x');
  std::ofstream(source / "data", std::ios::binary) << contents;
  const auto path = root.path() / "streamer.json";
  std::ofstream(path) << R"({"filesystem":{"strategies":["sync_buffered"],"queueDepth":"4",
      "chunkBytes":4096,"maxEngines":2},"readChunkBytes":2097152})";
  const auto options = ResolveModelStreamerOptions(ReadModelStreamerConfig(path));
  ConfigureModelStreamerEnvironment(options);
  ModelStreamerTransferEngine engine(root.path(), options);
  StorageBackend storage;
  storage.mutable_filesystem()->set_directory(source.string());
  for (const auto* destination : {"first", "second"}) {
    const auto target = root.path() / destination;
    engine.StageRestore(engine.PrepareRestore(storage), target);
    std::ifstream input(target / "data", std::ios::binary);
    const std::string actual(std::istreambuf_iterator<char>(input), {});
    EXPECT_EQ(actual, contents);
  }
}

TEST_F(ModelStreamerConfigTest, ReadsBoundedFiles)
{
  test::TemporaryDirectory root;
  const auto path = root.path() / "streamer.json";
  EXPECT_THROW(ReadModelStreamerConfig(path), std::invalid_argument);
  std::ofstream(path) << R"({"filesystem":{"strategies":["sync_buffered"]}})";
  EXPECT_EQ(ReadModelStreamerConfig(path).filesystem_strategy, "sync_buffered");
  std::ofstream(path) << std::string(65537, ' ');
  EXPECT_THROW(ReadModelStreamerConfig(path), std::invalid_argument);
}
}  // namespace
}  // namespace snapshot::pagebroker
