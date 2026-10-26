// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"

#include <gtest/gtest.h>

#include <string>

#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
TEST(ModelStreamerRestoreTest, RejectsUnsafeOrIncompleteRestoreInventories)
{
  test::TemporaryDirectory temporary;
  const fs::path root = temporary.path() / "destination";
  for (const auto& unsafe : {fs::path("../escape"), fs::path("/absolute"), fs::path("nested/../escape")}) {
    CheckpointInventory inventory;
    inventory.files.push_back(CheckpointFile{"s3://test-bucket/data", unsafe, 0});
    ModelStreamerRestore restore;
    EXPECT_THROW(restore.Stage(inventory, root), std::invalid_argument);
  }

  CheckpointInventory unordered;
  unordered.directories.push_back(CheckpointDirectory{"parent/child"});
  unordered.directories.push_back(CheckpointDirectory{"parent"});
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(unordered, root), std::invalid_argument);
  EXPECT_FALSE(fs::exists(root));
}

TEST(ModelStreamerRestoreTest, RejectsNonS3SourcesBeforeCreatingStaging)
{
  test::TemporaryDirectory root;
  ModelStreamerRestore restore;
  for (const auto& locator : {std::string("local"), std::string("/tmp/local"),
       std::string("file:///tmp/local"), std::string("https://bucket/key"),
       std::string("s3:///key"), std::string("s3://bucket/"), std::string("s3://bucket"),
       std::string("s3://bucket/key\0suffix", 22)}) {
    SCOPED_TRACE(locator);
    CheckpointInventory inventory;
    inventory.files.push_back({"s3://test-bucket/valid", "first", 0});
    inventory.files.push_back({locator, "second", 0});
    EXPECT_THROW(restore.Stage(inventory, root.path() / "unused"), std::invalid_argument);
    EXPECT_FALSE(fs::exists(root.path() / "unused"));
  }
}

TEST(ModelStreamerRestoreTest, RejectsDuplicateDirectoriesBeforeCreatingStaging)
{
  test::TemporaryDirectory root;
  CheckpointInventory inventory;
  inventory.directories = {{"parent"}, {"parent"}};
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, root.path() / "unused"), std::invalid_argument);
  EXPECT_FALSE(fs::exists(root.path() / "unused"));
}

TEST(ModelStreamerRestoreTest, RejectsDuplicateFilesBeforeCreatingStaging)
{
  test::TemporaryDirectory root;
  CheckpointInventory inventory;
  inventory.files = {{"s3://test-bucket/data", "data", 0}, {"s3://test-bucket/data", "data", 0}};
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, root.path() / "unused"), std::invalid_argument);
  EXPECT_FALSE(fs::exists(root.path() / "unused"));
}

TEST(ModelStreamerRestoreTest, RejectsFileDirectoryCollisionsBeforeCreatingStaging)
{
  test::TemporaryDirectory root;
  CheckpointInventory inventory;
  inventory.directories = {{"data"}};
  inventory.files = {{"s3://test-bucket/data", "data", 0}};
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, root.path() / "unused"), std::invalid_argument);
  EXPECT_FALSE(fs::exists(root.path() / "unused"));
}

TEST(ModelStreamerRestoreTest, RejectsFileAsParentBeforeCreatingStaging)
{
  test::TemporaryDirectory root;
  CheckpointInventory inventory;
  inventory.files = {{"s3://test-bucket/parent", "parent", 0}, {"s3://test-bucket/child", "parent/child", 0}};
  ModelStreamerRestore restore;
  EXPECT_THROW(restore.Stage(inventory, root.path() / "unused"), std::invalid_argument);
  EXPECT_FALSE(fs::exists(root.path() / "unused"));
}

TEST(ModelStreamerRestoreTest, CancellationAndDeadlineRejectBeforeCreatingStaging)
{
  test::TemporaryDirectory root;
  ModelStreamerRestore restore;
  CheckpointInventory inventory;
  inventory.files.push_back({"s3://test-bucket/empty", "data", 0});
  Cancellation cancellation;
  cancellation.Cancel();
  const auto destination = root.path() / "cancelled";
  EXPECT_THROW(restore.Stage(inventory, destination, cancellation), CancellationError);
  EXPECT_FALSE(fs::exists(destination));
  EXPECT_THROW(restore.Stage(inventory, destination, Cancellation(Cancellation::Clock::now())), CancellationError);
  EXPECT_FALSE(fs::exists(destination));
  EXPECT_NO_THROW(restore.Stage(inventory, root.path() / "retry"));
}

TEST(ModelStreamerRestoreTest, CreatesEmptyFilesAndDirectoriesWithoutReadingSource)
{
  test::TemporaryDirectory root;
  ModelStreamerRestore restore;
  CheckpointInventory inventory;
  inventory.directories.push_back({"empty", fs::perms::owner_all});
  inventory.files.push_back({"s3://test-bucket/missing-empty-object", "data", 0});
  EXPECT_NO_THROW(restore.Stage(inventory, root.path() / "restored"));
  EXPECT_EQ(fs::file_size(root.path() / "restored/data"), 0);
  EXPECT_TRUE(fs::is_empty(root.path() / "restored/empty"));
}
}  // namespace
}  // namespace snapshot::pagebroker
