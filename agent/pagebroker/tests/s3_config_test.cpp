// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "s3_config.hpp"

#include <gtest/gtest.h>
#include <nlohmann/json.hpp>

namespace snapshot::pagebroker {
namespace {
using Json = nlohmann::json;
const char* kConfig = R"({"storeId":"test-store","endpoint":"https://s3.example.test","region":"us-east-1","bucket":"checkpoint-bucket","prefix":"snapshots"})";

TEST(S3ConfigTest, HasFiniteDefaultsAndNoCredentialValues)
{
  const auto config = ParseS3Config(kConfig);
  EXPECT_EQ(config.prefix, "snapshots");
  EXPECT_EQ(config.transaction_lifetime, std::chrono::hours(2));
  EXPECT_EQ(config.transfer.upload_limits.buffer_budget, 128 * 1024 * 1024);
  EXPECT_TRUE(config.transfer.connection.access_key_id.empty());
}

TEST(S3ConfigTest, ConfiguresSdkRetriesAndConnectionTimeoutIndependently)
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
  EXPECT_EQ(config.transfer.restore_timeout, std::chrono::hours(2));
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
  EXPECT_THROW(ParseS3Config(value.dump()), Json::type_error);
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
