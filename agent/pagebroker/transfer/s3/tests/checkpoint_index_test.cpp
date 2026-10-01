// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/checkpoint_index.hpp"

#include <gtest/gtest.h>
#include <nlohmann/json.hpp>

namespace snapshot::pagebroker {
namespace {
namespace fs = std::filesystem;
using Json = nlohmann::json;

RestorePlan
Plan()
{
  RestorePlan plan;
  plan.root_permissions = fs::perms::owner_all;
  plan.directories.push_back({"nested", fs::perms::owner_all});
  plan.files.push_back({"", "manifest.yaml", 3, fs::perms::owner_read, utils::Sha256Digest{}});
  plan.files.push_back({"", "nested/empty", 0, fs::perms::owner_read, utils::Sha256Digest{}});
  return plan;
}

TEST(CheckpointIndexTest, RoundTripsOnlyFileInventory)
{
  const auto text = SerializeCheckpointIndex(Plan());
  EXPECT_EQ(SerializeCheckpointIndex(ParseCheckpointIndex(text)), text);
  EXPECT_EQ(ValidateCheckpointPlan(ParseCheckpointIndex(text)), 3);
  const auto json = Json::parse(text);
  EXPECT_EQ(json.size(), 4);
  EXPECT_FALSE(json.contains("storeID"));
  EXPECT_TRUE(ParseCheckpointIndex(text).files.front().source_locator.empty());
}

TEST(CheckpointIndexTest, RejectsUnsafePathsAndInvalidMetadata)
{
  const auto fixture = Json::parse(SerializeCheckpointIndex(Plan()));
  for (const auto* path : {"../escape", "/absolute", "a//b", "a/../b", "missing/parent", "nested", "manifest.yaml", "a\\b"}) {
    auto value = fixture;
    value["files"][1]["path"] = path;
    EXPECT_THROW(ParseCheckpointIndex(value.dump()), CheckpointError) << path;
  }
  for (const auto& change : std::vector<Json>{
           {{"sha256", "abc"}}, {{"sha256", std::string(64, 'A')}}, {{"mode", 04777}}, {{"size", -1}},
           {{"size", 0.5}}, {{"size", 18446744073709551615ULL}}, {{"extra", true}}}) {
    auto value = fixture;
    value["files"][0].update(change);
    EXPECT_THROW(ParseCheckpointIndex(value.dump()), CheckpointError);
  }
  auto duplicate = fixture.dump();
  duplicate.insert(1, "\"rootMode\":448,");
  EXPECT_THROW(ParseCheckpointIndex(duplicate), CheckpointError);
  auto value = fixture;
  value["files"].erase(0);
  EXPECT_THROW(ParseCheckpointIndex(value.dump()), CheckpointError);
  EXPECT_THROW(ParseCheckpointIndex(std::string(kMaxIndexBytes + 1, 'x')), CheckpointError);
  value = fixture;
  value["format"] = "snapshot.pagebroker/v1";
  try {
    ParseCheckpointIndex(value.dump());
    FAIL() << "old unmerged format accepted";
  }
  catch (const CheckpointError& error) {
    EXPECT_EQ(error.code, Failure::UNSUPPORTED_ARTIFACT);
  }
}

TEST(CheckpointIndexTest, ValidatesIDsAndAggregateSize)
{
  EXPECT_NO_THROW(ValidateCheckpointID(std::string(64, 'a')));
  for (const auto& id : {std::string(), std::string(63, 'a'), std::string(64, 'A'), std::string(64, 'g'), std::string("../capture")})
    EXPECT_THROW(ValidateCheckpointID(id), std::invalid_argument);
  auto plan = Plan();
  plan.files.back().size_bytes = kMaxCheckpointBytes;
  EXPECT_THROW(ValidateCheckpointPlan(plan), std::invalid_argument);
  plan = Plan();
  plan.files.front().expected_sha256.reset();
  EXPECT_THROW(ValidateCheckpointPlan(plan), std::invalid_argument);
  EXPECT_NO_THROW(ValidateCheckpointPlan(plan, false));
}

}  // namespace
}  // namespace snapshot::pagebroker
