// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"

#include <gtest/gtest.h>

#include <fstream>
#include <future>
#include <string>

#include "tests/temporary_directory.hpp"
#include "tests/file_contents.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
TEST(ModelStreamerRestoreTest, RejectsNonS3SourcesBeforeCreatingStaging)
{
  test::TemporaryDirectory root;
  ModelStreamerRestore restore;
  for (const auto& locator : {std::string("local"), std::string("/tmp/local"),
       std::string("file:///tmp/local"), std::string("https://bucket/key"),
       std::string("s3:///key"), std::string("s3://bucket/"), std::string("s3://bucket"),
       std::string("s3://bucket/key\0suffix", 22)}) {
    SCOPED_TRACE(locator);
    RestorePlan plan;
    plan.files.push_back({"s3://test-bucket/valid", "first", 0});
    plan.files.push_back({locator, "second", 0});
    EXPECT_THROW(restore.Stage(plan, root.path() / "unused"), std::invalid_argument);
    EXPECT_FALSE(fs::exists(root.path() / "unused"));
  }
}

TEST(ModelStreamerRestoreTest, CancellationAndDeadlineRejectBeforeCreatingStaging)
{
  test::TemporaryDirectory root;
  ModelStreamerRestore restore;
  RestorePlan plan;
  plan.files.push_back({"s3://test-bucket/empty", "data", 0});
  Cancellation cancellation;
  cancellation.Cancel();
  const auto destination = root.path() / "cancelled";
  EXPECT_THROW(restore.Stage(plan, destination, cancellation), CancellationError);
  EXPECT_FALSE(fs::exists(destination));
  EXPECT_THROW(restore.Stage(plan, destination, Cancellation(Cancellation::Clock::now())), CancellationError);
  EXPECT_FALSE(fs::exists(destination));
  EXPECT_NO_THROW(restore.Stage(plan, root.path() / "retry"));
}

TEST(ModelStreamerRestoreTest, CreatesEmptyFilesAndDirectoriesWithoutReadingSource)
{
  test::TemporaryDirectory root;
  ModelStreamerRestore restore;
  RestorePlan plan;
  plan.directories.push_back({"empty", fs::perms::owner_all});
  plan.files.push_back({"s3://test-bucket/missing-empty-object", "data", 0});
  EXPECT_NO_THROW(restore.Stage(plan, root.path() / "restored"));
  EXPECT_EQ(fs::file_size(root.path() / "restored/data"), 0);
  EXPECT_TRUE(fs::is_empty(root.path() / "restored/empty"));
}
}  // namespace
}  // namespace snapshot::pagebroker
