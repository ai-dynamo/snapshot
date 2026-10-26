// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_config.hpp"

#include <gtest/gtest.h>
#include <fstream>

#include "tests/temporary_directory.hpp"
#include <nlohmann/json.hpp>

namespace snapshot::pagebroker {
namespace {
using Json = nlohmann::json;
const char* kConfig = R"({"storeId":"test-store","endpoint":"https://s3.example.test","region":"us-east-1","bucket":"checkpoint-bucket","prefix":"snapshots"})";

TEST(S3ConfigTest, ReadsBoundedFiles)
{
  test::TemporaryDirectory root;
  const auto path = root.path() / "s3.json";
  EXPECT_THROW(ReadS3Config(path), std::invalid_argument);
  std::ofstream(path) << kConfig;
  EXPECT_EQ(ReadS3Config(path).bucket, "checkpoint-bucket");
  std::string text = kConfig;
  text.resize(65536, ' ');
  std::ofstream(path) << text;
  EXPECT_EQ(ReadS3Config(path).bucket, "checkpoint-bucket");
  std::ofstream(path) << text << ' ';
  EXPECT_THROW(ReadS3Config(path), std::invalid_argument);
  EXPECT_THROW(ReadS3Config(root.path()), std::invalid_argument);
}

TEST(S3ConfigTest, HasFiniteDefaultsAndNoCredentialValues)
{
  const auto config = ParseS3Config(kConfig);
  EXPECT_EQ(config.prefix, "snapshots");
  EXPECT_EQ(config.transaction_lifetime, std::chrono::hours(2));
  EXPECT_EQ(config.transfer.restore_timeout, std::chrono::minutes(20));
  EXPECT_EQ(config.transfer.upload_limits.part_size, 64ULL * 1024 * 1024);
  EXPECT_EQ(config.transfer.upload_limits.buffer_budget, 1024ULL * 1024 * 1024);
  EXPECT_TRUE(config.transfer.connection.access_key_id.empty());
}

TEST(S3ConfigTest, OptionalLimitsPreserveDefaults)
{
  const auto defaults = ParseS3Config(kConfig);
  for (const auto& limits : {Json::object(), Json{{"activeTransactions", 4}}}) {
    auto value = Json::parse(kConfig);
    value["limits"] = limits;
    const auto config = ParseS3Config(value.dump());
    EXPECT_EQ(config.transaction_lifetime, defaults.transaction_lifetime);
    EXPECT_EQ(config.transfer.upload_limits, defaults.transfer.upload_limits);
    EXPECT_EQ(config.transfer.restore_timeout, defaults.transfer.restore_timeout);
    EXPECT_EQ(config.staging_bytes, defaults.staging_bytes);
  }
}

TEST(S3ConfigTest, ReportsFieldsWithoutSuppliedValues)
{
  auto expect_error = [](const std::string& text, const char* field) {
    try {
      ParseS3Config(text);
      FAIL() << "accepted invalid configuration";
    }
    catch (const std::invalid_argument& error) {
      EXPECT_NE(std::string(error.what()).find(field), std::string::npos);
      EXPECT_EQ(std::string(error.what()).find("private-value"), std::string::npos);
    }
  };
  expect_error("{\"private-value\": invalid}", "JSON");
  for (const auto* field : {"storeId", "endpoint", "region", "bucket"}) {
    auto value = Json::parse(kConfig);
    value.erase(field);
    expect_error(value.dump(), field);
    value[field] = {"private-value"};
    expect_error(value.dump(), field);
  }
  for (const auto* field : {"prefix", "addressing", "caFile", "allowHttp"}) {
    auto value = Json::parse(kConfig);
    value[field] = {"private-value"};
    expect_error(value.dump(), field);
  }
  auto value = Json::parse(kConfig);
  value["limits"] = {{"requestSeconds", "private-value"}};
  expect_error(value.dump(), "requestSeconds");
  value["limits"] = {{"requestSeconds", 61}};
  expect_error(value.dump(), "requestSeconds");
  value["private-value"] = true;
  expect_error(value.dump(), "unknown S3 configuration field");
}

TEST(S3ConfigTest, CapsRestoreSubmissionsWithoutExtendingShorterTransactions)
{
  auto value = Json::parse(kConfig);
  for (const auto seconds : {120, 1200, 7200}) {
    SCOPED_TRACE(seconds);
    value["limits"] = {{"transactionSeconds", seconds}};
    const auto config = ParseS3Config(value.dump());
    EXPECT_EQ(config.transaction_lifetime, std::chrono::seconds(seconds));
    EXPECT_EQ(config.transfer.restore_timeout, std::chrono::seconds(seconds < 1200 ? seconds : 1200));
  }
}

TEST(S3ConfigTest, ConfiguresRcloneRetriesAndConnectionTimeoutIndependently)
{
  auto value = Json::parse(kConfig);
  value["limits"] = {{"requestSeconds", 2}};
  auto config = ParseS3Config(value.dump());
  EXPECT_EQ(config.transfer.upload_limits.connect_timeout, std::chrono::seconds(2));
  EXPECT_EQ(config.transfer.upload_limits.request_retries, 3);
  value["limits"]["uploadRequestRetries"] = 0;
  value["limits"]["uploadConnectSeconds"] = 1;
  config = ParseS3Config(value.dump());
  EXPECT_EQ(config.transfer.upload_limits.connect_timeout, std::chrono::seconds(1));
  EXPECT_EQ(config.transfer.upload_limits.request_timeout, std::chrono::seconds(2));
  EXPECT_EQ(config.transfer.upload_limits.request_retries, 0);
  EXPECT_EQ(config.transfer.restore_timeout, std::chrono::minutes(20));
  for (const auto* field : {"uploadRequestRetries", "uploadConnectSeconds"}) {
    auto invalid = value;
    invalid["limits"][field] = -1;
    EXPECT_THROW(ParseS3Config(invalid.dump()), std::invalid_argument);
    invalid["limits"][field] = 11;
    EXPECT_THROW(ParseS3Config(invalid.dump()), std::invalid_argument);
  }
  value["limits"]["uploadConnectSeconds"] = 0;
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
}

TEST(S3ConfigTest, BoundsAggregateRcloneBuffersAndRejectsObsoleteSettings)
{
  auto value = Json::parse(kConfig);
  value["limits"] = {{"uploadPartBytes", 8 * 1024 * 1024},
                     {"uploadBufferBytes", 27 * 1024 * 1024}, {"uploadActiveFiles", 3}};
  const auto config = ParseS3Config(value.dump());
  EXPECT_EQ(config.transfer.upload_limits.buffer_budget, 27U * 1024 * 1024);
  EXPECT_EQ(config.transfer.upload_limits.active_files, 3U);
  value["limits"]["uploadBufferBytes"] = 27 * 1024 * 1024 - 1;
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  value["limits"] = {{"uploadPartBytes", 8 * 1024 * 1024},
                     {"uploadBufferBytes", 9 * 1024 * 1024}, {"uploadActiveFiles", 1}};
  EXPECT_NO_THROW(ParseS3Config(value.dump()));
  for (const auto* field : {"uploadTargetGbps", "uploadWorkers"}) {
    value["limits"] = {{field, 4}};
    EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  }
  value["limits"] = {{"uploadPartBytes", 5 * 1024 * 1024}};
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
}

TEST(S3ConfigTest, ValidatesConnectionFieldsBeforeOptionalSettings)
{
  for (const auto* field : {"region", "endpoint"}) {
    auto value = Json::parse(kConfig);
    value[field] = "";
    // Missing required connection settings must be rejected before interpreting
    // an optional field whose type is itself invalid.
    value["allowHttp"] = "invalid-boolean";
    EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  }
  auto value = Json::parse(kConfig);
  value["allowHttp"] = "invalid-boolean";
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  value = Json::parse(kConfig);
  value["caFile"] = "relative/ca.pem";
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  value["caFile"] = "/absolute/ca.pem";
  EXPECT_NO_THROW(ParseS3Config(value.dump()));
}

TEST(S3ConfigTest, RejectsAmbiguousAndUnboundedConfiguration)
{
  auto value = Json::parse(kConfig);
  value["limits"] = {{"transactionSeconds", 0}};
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  value["limits"] = {{"stagingBytes", -1}};
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  value["limits"] = {{"uploadBufferBytes", 1}};
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  value = Json::parse(kConfig);
  value["credentials"] = {{"file", "/ignored"}};
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  std::string duplicate = kConfig;
  duplicate.insert(1, "\"region\":\"other\",");
  EXPECT_THROW(ParseS3Config(duplicate), std::invalid_argument);
  for (const auto* prefix : {"/absolute", "a/../b", "a//b", "a\\b", "a\nb"}) {
    value = Json::parse(kConfig);
    value["prefix"] = prefix;
    EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  }
  value = Json::parse(kConfig);
  value["endpoint"] = "http://localhost:9000";
  EXPECT_THROW(ParseS3Config(value.dump()), std::invalid_argument);
  value["allowHttp"] = true;
  EXPECT_NO_THROW(ParseS3Config(value.dump()));
  EXPECT_THROW(ParseS3Config(std::string(65537, 'x')), std::invalid_argument);
}
}  // namespace
}  // namespace snapshot::pagebroker
