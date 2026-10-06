// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "daemon_options.hpp"

#include <gtest/gtest.h>

#include <vector>

namespace {
std::optional<DaemonOptions>
Parse(std::initializer_list<const char*> flags)
{
  std::vector<const char*> args{"pagebroker", "/socket", "/staging", "/storage"};
  args.insert(args.end(), flags);
  return ParseDaemonArguments(args.size(), args.data());
}

TEST(DaemonOptionsTest, AcceptsExistingAndIndependentConfigurationInputs)
{
  auto options = Parse({"--max-concurrent-requests", "16"});
  ASSERT_TRUE(options);
  EXPECT_EQ(options->max_concurrent_requests, 16);
  EXPECT_TRUE(options->storage_config.empty());
  EXPECT_TRUE(options->model_streamer_config.empty());
  EXPECT_TRUE(Parse({"--max-concurrent-requests", "16", "--storage-config", "s3.json"}));
  EXPECT_TRUE(Parse({"--max-concurrent-requests", "16", "--model-streamer-config", "streamer.json"}));
  for (bool storage_first : {false, true}) {
    const char* first = storage_first ? "--storage-config" : "--model-streamer-config";
    const char* second = storage_first ? "--model-streamer-config" : "--storage-config";
    options = Parse({first, "first.json", "--max-concurrent-requests", "16", second, "second.json"});
    ASSERT_TRUE(options);
    EXPECT_EQ(options->storage_config, storage_first ? "first.json" : "second.json");
    EXPECT_EQ(options->model_streamer_config, storage_first ? "second.json" : "first.json");
  }
}

TEST(DaemonOptionsTest, RejectsMissingDuplicateAndInvalidArguments)
{
  EXPECT_FALSE(Parse({}));
  EXPECT_FALSE(Parse({"--storage-config", "s3.json"}));
  for (const auto* value : {"", "0", "-1", "1x", "18446744073709551616"})
    EXPECT_FALSE(Parse({"--max-concurrent-requests", value}));
  EXPECT_FALSE(Parse({"--max-concurrent-requests", "16", "--storage-config"}));
  EXPECT_FALSE(Parse({"--max-concurrent-requests", "16", "--model-streamer-config", ""}));
  EXPECT_FALSE(Parse({"--max-concurrent-requests", "16", "--unknown", "value"}));
  EXPECT_FALSE(Parse({"--max-concurrent-requests", "16", "--max-concurrent-requests", "17"}));
  EXPECT_FALSE(Parse({"--max-concurrent-requests", "16", "--storage-config", "a", "--storage-config", "b"}));
  EXPECT_FALSE(Parse({"--max-concurrent-requests", "16", "--model-streamer-config", "a", "--model-streamer-config", "b"}));
}
}  // namespace
