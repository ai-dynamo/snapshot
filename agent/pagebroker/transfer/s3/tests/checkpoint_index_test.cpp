// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/s3/checkpoint_index.hpp"

#include <gtest/gtest.h>
#include <nlohmann/json.hpp>

#include <algorithm>
#include <stdexcept>
#include <string>
#include <vector>

namespace snapshot::pagebroker {
namespace {
namespace fs = std::filesystem;
using Json = nlohmann::json;

CheckpointInventory
Inventory()
{
  CheckpointInventory inventory;
  inventory.root_permissions = fs::perms::owner_all;
  inventory.directories.push_back({"nested", fs::perms::owner_all});
  inventory.files.push_back({"", "manifest.yaml", 3, fs::perms::owner_read});
  inventory.files.push_back({"", "nested/empty", 0, fs::perms::owner_read});
  return inventory;
}

TEST(CheckpointIndexTest, RoundTripsOnlyFileInventory)
{
  const auto text = SerializeCheckpointIndex(Inventory());
  EXPECT_EQ(SerializeCheckpointIndex(ParseCheckpointIndex(text)), text);
  EXPECT_EQ(ValidateCheckpointInventory(ParseCheckpointIndex(text)), 3);
  const auto json = Json::parse(text);
  EXPECT_EQ(json.size(), 4);
  EXPECT_EQ(json.at("format"), "snapshot.s3-checkpoint/v1");
  for (const auto& file : json.at("files")) {
    EXPECT_EQ(file.size(), 3);
    EXPECT_TRUE(file.contains("path") && file.contains("mode") && file.contains("size"));
    EXPECT_FALSE(file.contains("sha256"));
  }
  EXPECT_FALSE(json.contains("storeID"));
  EXPECT_TRUE(ParseCheckpointIndex(text).files.front().source_locator.empty());
}

TEST(CheckpointIndexTest, ParsesMaximumEntryCountWithinStructuralBudget)
{
  auto inventory = Inventory();
  inventory.directories.clear();
  inventory.files.resize(1);
  for (std::size_t i = 1; i < kMaxCheckpointEntries; ++i)
    inventory.files.push_back({"", "file-" + std::to_string(i), 0, fs::perms::owner_read});
  std::sort(inventory.files.begin(), inventory.files.end(),
      [](const auto& a, const auto& b) { return a.relative_path < b.relative_path; });
  const auto text = SerializeCheckpointIndex(inventory);
  EXPECT_EQ(ParseCheckpointIndex(text).files.size(), kMaxCheckpointEntries);
}

TEST(CheckpointIndexTest, RejectsUnsafePathsAndInvalidMetadata)
{
  const auto fixture = Json::parse(SerializeCheckpointIndex(Inventory()));
  for (const auto* path : {"../escape", "/absolute", "a//b", "a/../b", "missing/parent", "nested", "manifest.yaml", "a\\b"}) {
    auto value = fixture;
    value["files"][1]["path"] = path;
    EXPECT_THROW(ParseCheckpointIndex(value.dump()), CheckpointError) << path;
  }
  for (const auto& change : std::vector<Json>{
           {{"mode", 04777}}, {{"size", -1}},
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
}

TEST(CheckpointIndexTest, RejectsInvalidDirectoryTrees)
{
  const auto fixture = Json::parse(SerializeCheckpointIndex(Inventory()));
  for (const auto& paths : std::vector<std::vector<std::string>>{
           {"nested", "nested"}, {"nested/child", "nested"}, {"missing/child"}, {"../escape"}, {"/absolute"}}) {
    auto value = fixture;
    value["files"].erase(1);
    value["directories"] = Json::array();
    for (const auto& path : paths)
      value["directories"].push_back({{"path", path}, {"mode", 0700}});
    SCOPED_TRACE(value.dump());
    try {
      ParseCheckpointIndex(value.dump());
      FAIL() << "invalid directory tree accepted";
    }
    catch (const CheckpointError& error) {
      EXPECT_EQ(error.code, Failure::ARTIFACT_CORRUPT);
    }
  }
}

TEST(CheckpointIndexTest, RejectsFileAsParent)
{
  auto value = Json::parse(SerializeCheckpointIndex(Inventory()));
  value["files"][1]["path"] = "manifest.yaml/child";
  EXPECT_THROW(ParseCheckpointIndex(value.dump()), CheckpointError);
}

TEST(CheckpointIndexTest, ValidatesIDsAndAggregateSize)
{
  EXPECT_NO_THROW(ValidateCheckpointID(std::string(64, 'a')));
  for (const auto& id : {std::string(), std::string(63, 'a'), std::string(64, 'A'), std::string(64, 'g'), std::string("../capture")})
    EXPECT_THROW(ValidateCheckpointID(id), std::invalid_argument);
  auto inventory = Inventory();
  inventory.files.back().size_bytes = kMaxCheckpointBytes;
  EXPECT_THROW(ValidateCheckpointInventory(inventory), std::invalid_argument);
}

TEST(CheckpointIndexTest, RejectsUnknownFormats)
{
  for (const auto* format : {"snapshot.pagebroker/v1", "snapshot.s3-checkpoint/v99"}) {
    auto value = Json::parse(SerializeCheckpointIndex(Inventory()));
    value["format"] = format;
    try {
      ParseCheckpointIndex(value.dump());
      FAIL() << "unsupported format accepted: " << format;
    }
    catch (const CheckpointError& error) {
      EXPECT_EQ(error.code, Failure::UNSUPPORTED_ARTIFACT);
    }
  }
}

TEST(CheckpointIndexTest, RejectsMalformedFormatTypesAsCorrupt)
{
  const auto fixture = Json::parse(SerializeCheckpointIndex(Inventory()));
  for (const auto& format : std::vector<Json>{nullptr, 1, true, Json::array(), Json::object()}) {
    SCOPED_TRACE(format.dump());
    auto value = fixture;
    value["format"] = format;
    try {
      ParseCheckpointIndex(value.dump());
      FAIL() << "malformed format accepted";
    }
    catch (const CheckpointError& error) {
      EXPECT_EQ(error.code, Failure::ARTIFACT_CORRUPT);
    }
  }
}

TEST(CheckpointIndexTest, RejectsInvalidUtf8Paths)
{
  auto inventory = Inventory();
  inventory.files.back().relative_path = "nested/invalid-\xff";
  EXPECT_THROW(ValidateCheckpointInventory(inventory), std::invalid_argument);
}

TEST(CheckpointIndexTest, RejectsUnexpectedFileFields)
{
  auto value = Json::parse(SerializeCheckpointIndex(Inventory()));
  value["files"][0]["sha256"] = std::string(64, '0');
  try {
    ParseCheckpointIndex(value.dump());
    FAIL() << "unexpected digest field accepted";
  }
  catch (const CheckpointError& error) {
    EXPECT_EQ(error.code, Failure::ARTIFACT_CORRUPT);
  }
}

TEST(CheckpointIndexTest, PreservesManifestAndTreeBounds)
{
  auto inventory = Inventory();
  inventory.files.front().size_bytes = 0;
  EXPECT_THROW(ValidateCheckpointInventory(inventory), std::invalid_argument);
  inventory.files.front().size_bytes = kMaxManifestBytes + 1;
  EXPECT_THROW(ValidateCheckpointInventory(inventory), std::invalid_argument);
  inventory = Inventory();
  inventory.directories.resize(kMaxCheckpointEntries);
  EXPECT_THROW(ValidateCheckpointInventory(inventory), std::invalid_argument);
  inventory = Inventory();
  inventory.files.back().relative_path = std::string(513, 'x');
  EXPECT_THROW(ValidateCheckpointInventory(inventory), std::invalid_argument);
}

}  // namespace
}  // namespace snapshot::pagebroker
