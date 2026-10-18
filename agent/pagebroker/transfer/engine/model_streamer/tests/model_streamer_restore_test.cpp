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
class RestoreContentsTest : public ::testing::Test {
 protected:
  RestorePlan Plan(const std::string& contents)
  {
    const auto source = root_.path() / "source";
    std::ofstream(source, std::ios::binary).write(contents.data(), contents.size());
    RestorePlan plan;
    plan.root_permissions = fs::perms::owner_all;
    plan.directories.push_back({"nested", fs::perms::owner_read | fs::perms::owner_exec});
    plan.directories.push_back({"empty", fs::perms::owner_all});
    plan.files.push_back({source.string(), "nested/data", contents.size(), fs::perms::none});
    return plan;
  }

  test::TemporaryDirectory root_;
  ModelStreamerRestore restore_{std::chrono::seconds(5)};
};

TEST_F(RestoreContentsTest, CancellationAndDeadlineRejectBeforeCreatingStaging)
{
  const auto plan = Plan("data");
  std::stop_source cancellation;
  cancellation.request_stop();
  const auto destination = root_.path() / "cancelled";
  EXPECT_THROW(restore_.Stage(plan, destination, {TransferControl::Clock::time_point::max(), cancellation.get_token()}), TransferInterrupted);
  EXPECT_FALSE(fs::exists(destination));
  EXPECT_THROW(restore_.Stage(plan, destination, {TransferControl::Clock::now(), {}}), TransferInterrupted);
  EXPECT_FALSE(fs::exists(destination));
  EXPECT_NO_THROW(restore_.Stage(plan, root_.path() / "retry"));
}

TEST_F(RestoreContentsTest, RestoresBinaryContentsAndRestrictivePermissions)
{
  const auto plan = Plan(std::string("binary\0data", 11));
  const auto destination = root_.path() / "restored";
  ASSERT_NO_THROW(restore_.Stage(plan, destination));
  EXPECT_EQ(fs::status(destination / "nested/data").permissions(), fs::perms::none);
  EXPECT_EQ(fs::status(destination / "nested").permissions(), plan.directories.front().permissions);
  EXPECT_TRUE(fs::is_empty(destination / "empty"));
  fs::permissions(destination / "nested/data", fs::perms::owner_read);
  EXPECT_TRUE(test::FilesEqual(plan.files.front().source_locator, destination / "nested/data"));
}

TEST_F(RestoreContentsTest, RejectsTruncatedSource)
{
  const auto plan = Plan("original");
  std::ofstream(plan.files.front().source_locator) << "short";
  EXPECT_THROW(restore_.Stage(plan, root_.path() / "restored"), std::runtime_error);
}

TEST_F(RestoreContentsTest, CreatesEmptyFilesWithoutReadingSource)
{
  auto plan = Plan("");
  fs::remove(plan.files.front().source_locator);
  EXPECT_NO_THROW(restore_.Stage(plan, root_.path() / "empty-success"));
  EXPECT_EQ(fs::file_size(root_.path() / "empty-success/nested/data"), 0);
}

TEST_F(RestoreContentsTest, SupportsConcurrentRestores)
{
  const auto plan = Plan("concurrent data");
  auto first = std::async(std::launch::async, [&] { restore_.Stage(plan, root_.path() / "first"); });
  auto second = std::async(std::launch::async, [&] { restore_.Stage(plan, root_.path() / "second"); });
  EXPECT_NO_THROW(first.get());
  EXPECT_NO_THROW(second.get());
}
}  // namespace
}  // namespace snapshot::pagebroker
