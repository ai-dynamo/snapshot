// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/model_streamer/model_streamer_restore.hpp"

#include <gtest/gtest.h>

#include "tests/temporary_directory.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
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
