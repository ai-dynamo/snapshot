// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "model_streamer_restore.hpp"

#include <gtest/gtest.h>

#include <fstream>
#include <future>
#include <string>

#include "tests/temporary_directory.hpp"
#include "utils/sha256.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;
namespace {
class VerifiedRestoreTest : public ::testing::Test {
 protected:
  RestorePlan Plan(const std::string& contents, bool verify = true)
  {
    const auto source = root_.path() / "source";
    std::ofstream(source, std::ios::binary).write(contents.data(), contents.size());
    RestorePlan plan;
    plan.root_permissions = fs::perms::owner_all;
    plan.directories.push_back({"nested", fs::perms::owner_read | fs::perms::owner_exec});
    plan.directories.push_back({"empty", fs::perms::owner_all});
    plan.files.push_back({source.string(), "nested/data", contents.size(), fs::perms::none});
    if (verify)
      plan.files.back().expected_sha256 = utils::ComputeFileSha256(source);
    return plan;
  }

  test::TemporaryDirectory root_;
  ModelStreamerRestore restore_{std::chrono::seconds(5)};
};

TEST_F(VerifiedRestoreTest, VerifiesBeforeApplyingRestrictivePermissions)
{
  const auto plan = Plan(std::string("binary\0data", 11));
  const auto destination = root_.path() / "restored";
  ASSERT_NO_THROW(restore_.Stage(plan, destination));
  EXPECT_EQ(fs::status(destination / "nested/data").permissions(), fs::perms::none);
  EXPECT_EQ(fs::status(destination / "nested").permissions(), plan.directories.front().permissions);
  EXPECT_TRUE(fs::is_empty(destination / "empty"));
  fs::permissions(destination / "nested/data", fs::perms::owner_read);
  EXPECT_EQ(utils::ComputeFileSha256(destination / "nested/data"), plan.files.front().expected_sha256);
}

TEST_F(VerifiedRestoreTest, RejectsSameSizeCorruptionAndKeepsSessionUsable)
{
  auto plan = Plan("original");
  std::ofstream(plan.files.front().source_locator) << "modified";
  const auto destination = root_.path() / "failed";
  try {
    restore_.Stage(plan, destination);
    FAIL() << "corrupted contents were accepted";
  }
  catch (const std::runtime_error& error) {
    EXPECT_NE(std::string(error.what()).find("SHA-256 mismatch"), std::string::npos);
  }
  EXPECT_FALSE(restore_.Failed());
  // Failed staging is still owned by the caller, with accessible permissions.
  EXPECT_TRUE(fs::exists(destination / "nested/data"));
  EXPECT_EQ(fs::status(destination / "nested/data").permissions(), fs::perms::owner_read | fs::perms::owner_write);
  plan.files.front().expected_sha256 = utils::ComputeFileSha256(plan.files.front().source_locator);
  EXPECT_NO_THROW(restore_.Stage(plan, root_.path() / "retry"));
}

TEST_F(VerifiedRestoreTest, RejectsTruncatedSource)
{
  const auto plan = Plan("original");
  std::ofstream(plan.files.front().source_locator) << "short";
  EXPECT_THROW(restore_.Stage(plan, root_.path() / "restored"), std::runtime_error);
}

TEST_F(VerifiedRestoreTest, VerifiesEmptyFilesWithoutReadingSource)
{
  auto plan = Plan("");
  fs::remove(plan.files.front().source_locator);
  EXPECT_NO_THROW(restore_.Stage(plan, root_.path() / "empty-success"));
  plan.files.front().expected_sha256->front() ^= 1;
  EXPECT_THROW(restore_.Stage(plan, root_.path() / "empty-failure"), std::runtime_error);
}

TEST_F(VerifiedRestoreTest, PreservesOptionalDigestAndConcurrentRestores)
{
  const auto plan = Plan("no digest", false);
  auto first = std::async(std::launch::async, [&] { restore_.Stage(plan, root_.path() / "first"); });
  auto second = std::async(std::launch::async, [&] { restore_.Stage(plan, root_.path() / "second"); });
  EXPECT_NO_THROW(first.get());
  EXPECT_NO_THROW(second.get());
}
}  // namespace
}  // namespace snapshot::pagebroker
